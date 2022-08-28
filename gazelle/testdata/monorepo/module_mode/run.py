import os

import boto3
from bing import bing
from ping import ping
from ping.pong import pong, pong_test_util, pong_util

_ = boto3

if __name__ == "__main__":
    INIT_FILENAME = "__init__.py"
    dirname = os.path.dirname(os.path.abspath(__file__))
    assert bing() == os.path.join(dirname, "bing", INIT_FILENAME)
    assert ping() == os.path.join(dirname, "ping", INIT_FILENAME)
    assert pong() == os.path.join(dirname, "ping", "pong", INIT_FILENAME)
    assert pong_util() == os.path.join(dirname, "ping", "pong", "util.py")
    assert pong_test_util() == os.path.join(dirname, "ping", "pong", "test_util.py")
