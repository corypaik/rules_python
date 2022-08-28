import os

import boto3
import ping.util

_ = boto3


def ping():
    return os.path.abspath(__file__)
