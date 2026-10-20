from pathlib import Path


def pytest_addoption(parser):
    parser.addoption("--capture-dir", type=Path, default=None,
                     help="Explicit existing NHA-045 live evidence directory")
