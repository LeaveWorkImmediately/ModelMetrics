"""Bounded DCMI inventory adapter. Never substitutes a logic/card index for physical ID.

ABI source: ascend-common/devmanager/dcmi/dcmi_interface_api.h at
mind-cluster 7537ed628111e89f82e4823cd36b2c983d86ab6b. Go owns the process timeout.
"""
import ctypes
import json
import os


def inventory(library) -> list[dict]:
    signatures = {
        "dcmi_init": [],
        "dcmi_get_card_list": [ctypes.POINTER(ctypes.c_int), ctypes.POINTER(ctypes.c_int), ctypes.c_int],
        "dcmi_get_device_num_in_card": [ctypes.c_int, ctypes.POINTER(ctypes.c_int)],
        "dcmi_get_device_logic_id": [ctypes.POINTER(ctypes.c_int), ctypes.c_int, ctypes.c_int],
        "dcmi_get_device_phyid_from_logicid": [ctypes.c_uint, ctypes.POINTER(ctypes.c_uint)],
    }
    for name, arguments in signatures.items():
        function = getattr(library, name)
        function.argtypes, function.restype = arguments, ctypes.c_int

    def call(name, *arguments):
        code = getattr(library, name)(*arguments)
        if code != 0:
            raise RuntimeError(f"{name} failed: {code}")

    call("dcmi_init")
    count = ctypes.c_int()
    cards = (ctypes.c_int * 64)()
    call("dcmi_get_card_list", ctypes.byref(count), cards, 64)
    if not 0 <= count.value <= 64:
        raise RuntimeError("Invalid DCMI card count")
    devices = []
    seen = set()
    for card in list(cards)[:count.value]:
        chips = ctypes.c_int()
        call("dcmi_get_device_num_in_card", card, ctypes.byref(chips))
        if not 0 <= chips.value <= 32:
            raise RuntimeError("Invalid DCMI chip count")
        for chip in range(chips.value):
            logic = ctypes.c_int()
            call("dcmi_get_device_logic_id", ctypes.byref(logic), card, chip)
            if logic.value < 0:
                raise RuntimeError("Invalid logic ID")
            physical = ctypes.c_uint()
            call("dcmi_get_device_phyid_from_logicid", logic.value, ctypes.byref(physical))
            identity = str(physical.value)
            if identity in seen or len(devices) >= 256:
                raise RuntimeError("Duplicate identity or inventory capacity exceeded")
            seen.add(identity)
            devices.append({"physicalId": identity, "name": f"Ascend {identity}",
                            "model": os.environ.get("ASCEND_MODEL", "unknown"), "vendor": "Ascend"})
    return devices


if __name__ == "__main__":
    driver = ctypes.CDLL(os.environ.get("DCMI_LIBRARY_PATH", "/usr/local/Ascend/driver/lib64/driver/libdcmi.so"))
    print(json.dumps(inventory(driver)))
