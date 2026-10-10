FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod main.go main_test.go ./
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -o /model-device-agent .
FROM python:3.12-slim-bookworm
COPY --from=build /model-device-agent /usr/local/bin/model-device-agent
COPY dcmi_inventory.py /usr/local/lib/model-device-agent/dcmi_inventory.py
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/model-device-agent"]
