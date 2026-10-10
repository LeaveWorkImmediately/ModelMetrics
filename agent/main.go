// Node-bound inventory collector. Numeric performance samples remain in Prometheus.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type device struct {
	PhysicalID string `json:"physicalId"`
	Name       string `json:"name"`
	Model      string `json:"model"`
	Vendor     string `json:"vendor"`
}

type inventory struct {
	SchemaVersion string    `json:"schemaVersion"`
	ClusterID     string    `json:"clusterId"`
	NodeUID       string    `json:"nodeUid"`
	ObservedAt    time.Time `json:"observedAt"`
	Scope         string    `json:"scope"`
	Complete      bool      `json:"complete"`
	ProfileID     string    `json:"profileId"`
	Devices       []device  `json:"devices"`
}

func collect(ctx context.Context) ([]device, error) {
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(deadline, env("DRIVER_ADAPTER_PYTHON", "python3"), env("DRIVER_ADAPTER_PATH", "/usr/local/lib/model-device-agent/dcmi_inventory.py"))
	var output bytes.Buffer
	command.Stdout = &boundedWriter{writer: &output, remaining: 1024 * 1024}
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("DCMI inventory failed: %w", err)
	}
	return decodeInventory(&output)
}

func decodeInventory(reader io.Reader) ([]device, error) {
	var devices []device
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&devices); err != nil {
		return nil, err
	}
	if devices == nil || len(devices) > 256 {
		return nil, errors.New("invalid inventory size")
	}
	seen := map[string]bool{}
	for _, d := range devices {
		if d.PhysicalID == "" || d.Name == "" || seen[d.PhysicalID] {
			return nil, errors.New("invalid or duplicate physical identity")
		}
		seen[d.PhysicalID] = true
	}
	return devices, nil
}

type boundedWriter struct {
	writer    io.Writer
	remaining int
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	if len(data) > writer.remaining {
		return 0, errors.New("collector output exceeds budget")
	}
	writer.remaining -= len(data)
	return writer.writer.Write(data)
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type agent struct {
	client                                *http.Client
	base, tokenFile, node, epoch, session string
	sequence                              uint64
}

func (a *agent) request(ctx context.Context, method, path string, payload interface{}, response interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(data) > 2*1024*1024 {
		return errors.New("snapshot exceeds budget")
	}
	token, err := os.ReadFile(a.tokenFile)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, a.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/json")
	result, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer result.Body.Close()
	if result.StatusCode == 409 {
		a.session = ""
		freshEpoch := make([]byte, 16)
		if _, err := rand.Read(freshEpoch); err != nil {
			return err
		}
		a.epoch = hex.EncodeToString(freshEpoch)
		return errors.New("session/snapshot conflict; register again")
	}
	if result.StatusCode != 200 {
		return fmt.Errorf("Dashboard rejected request: HTTP %d", result.StatusCode)
	}
	if response != nil {
		return json.NewDecoder(io.LimitReader(result.Body, 2*1024*1024)).Decode(response)
	}
	return nil
}

func (a *agent) send(ctx context.Context, snapshot inventory) error {
	if a.session == "" {
		var response struct {
			SessionID string `json:"sessionId"`
		}
		if err := a.request(ctx, "POST", "/internal/v1/agent-sessions", map[string]string{"nodeUid": a.node, "agentEpoch": a.epoch}, &response); err != nil {
			return err
		}
		if response.SessionID == "" {
			return errors.New("missing session ID")
		}
		a.session, a.sequence = response.SessionID, 1
	}
	path := fmt.Sprintf("/internal/v1/agent-sessions/%s/snapshots/%d", a.session, a.sequence)
	if err := a.request(ctx, "PUT", path, snapshot, nil); err != nil {
		return err
	}
	a.sequence++
	return nil
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nodeUID(ctx context.Context) (string, error) {
	if value := os.Getenv("NODE_UID"); value != "" {
		return value, nil
	}
	name := os.Getenv("NODE_NAME")
	if name == "" {
		return "", errors.New("NODE_NAME is required")
	}
	ca, err := os.ReadFile(env("KUBERNETES_CA", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"))
	if err != nil {
		return "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return "", errors.New("invalid Kubernetes CA")
	}
	token, err := os.ReadFile(env("KUBERNETES_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token"))
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, "GET", "https://kubernetes.default.svc/api/v1/nodes/"+name, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", fmt.Errorf("node identity lookup HTTP %d", response.StatusCode)
	}
	var value struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&value)
	if err != nil || value.Metadata.UID == "" {
		return "", errors.New("missing node UID")
	}
	return value.Metadata.UID, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if os.Getenv("CLUSTER_ID") == "" || os.Getenv("DASHBOARD_URL") == "" {
		log.Fatal("CLUSTER_ID and DASHBOARD_URL are required")
	}
	if os.Getenv("INVENTORY_PROFILE_VERIFIED") != "true" {
		log.Fatal("Verify DCMI physical IDs against exporter IDs on this driver before enabling the inventory profile")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		log.Fatal(err)
	}
	uid, err := nodeUID(ctx)
	if err != nil {
		log.Fatalf("node identity unavailable: %v", err)
	}
	var lastSuccess atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if lastSuccess.Load() == 0 || time.Now().Unix()-lastSuccess.Load() > 900 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	health := &http.Server{Addr: ":8081", Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("health server failed: %v", err)
			stop()
		}
	}()
	defer health.Close()
	a := agent{client: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{}}, base: strings.TrimRight(os.Getenv("DASHBOARD_URL"), "/"), tokenFile: env("AGENT_TOKEN_FILE", "/var/run/device-token/token"), node: uid, epoch: hex.EncodeToString(random)}
	backoff := time.Second
	for ctx.Err() == nil {
		devices, err := collect(ctx)
		if err != nil {
			log.Printf("inventory unavailable: %v", err)
			if !sleep(ctx, 30*time.Second) {
				return
			}
			continue
		}
		snapshot := inventory{"device-observation.modelpilot.io/v1alpha1", os.Getenv("CLUSTER_ID"), a.node, time.Now().UTC(), "inventory", true, "ascend-dcmi-physical-v1", devices}
		// One bounded pending snapshot: retry exact payload/sequence; after 10 minutes
		// discard with an explicit log. Never replay old inventory as current state.
		started := time.Now()
		for {
			err = a.send(ctx, snapshot)
			if err == nil {
				lastSuccess.Store(time.Now().Unix())
				backoff = time.Second
				break
			}
			log.Printf("inventory delivery failed: %v", err)
			if time.Since(started) > 10*time.Minute {
				log.Print("pending inventory expired; observation gap")
				break
			}
			jitter := make([]byte, 1)
			_, _ = rand.Read(jitter)
			if !sleep(ctx, backoff+time.Duration(jitter[0])*time.Millisecond) {
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}
		if !sleep(ctx, 300*time.Second) {
			return
		}
	}
}
