import importlib.util
from pathlib import Path

import pytest

spec = importlib.util.spec_from_file_location("dcmi_inventory", Path(__file__).resolve().parents[1] / "agent/dcmi_inventory.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class Function:
    def __init__(self, call):
        self.call = call

    def __call__(self, *arguments):
        return self.call(*arguments)


class Driver:
    def __init__(self):
        self.dcmi_init = Function(lambda: 0)
        self.dcmi_get_card_list = Function(self.cards)
        self.dcmi_get_device_num_in_card = Function(lambda card, chips: self.output(chips, 4))
        self.dcmi_get_device_logic_id = Function(lambda logic, card, chip: self.output(logic, card * 10 + chip))
        self.dcmi_get_device_phyid_from_logicid = Function(lambda logic, physical: self.output(physical, logic + 100))

    @staticmethod
    def output(pointer, value):
        pointer._obj.value = value
        return 0

    def cards(self, count, cards, capacity):
        assert capacity == 64
        cards[0], cards[1] = 0, 4
        return self.output(count, 2)


def test_eight_chips_use_dcmi_physical_ids_instead_of_card_or_logic_indexes():
    devices = module.inventory(Driver())
    assert [device["physicalId"] for device in devices] == ["100", "101", "102", "103", "140", "141", "142", "143"]


def test_driver_failure_never_returns_a_complete_partial_or_empty_inventory():
    driver = Driver()
    driver.dcmi_get_device_phyid_from_logicid = Function(lambda logic, physical: 17)
    with pytest.raises(RuntimeError, match="failed: 17"):
        module.inventory(driver)


def test_duplicate_physical_ids_are_rejected():
    driver = Driver()
    driver.dcmi_get_device_phyid_from_logicid = Function(lambda logic, physical: driver.output(physical, 0))
    with pytest.raises(RuntimeError, match="Duplicate identity"):
        module.inventory(driver)


def test_verified_driver_can_report_zero_cards():
    driver = Driver()
    driver.dcmi_get_card_list = Function(lambda count, cards, capacity: driver.output(count, 0))
    assert module.inventory(driver) == []
