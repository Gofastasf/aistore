# coding: utf-8

"""
    DFC

    DFC is a scalable object-storage based caching system with Amazon and Google Cloud backends.  # noqa: E501

    OpenAPI spec version: 1.1.0
    Contact: dfc-jenkins@nvidia.com
    Generated by: https://openapi-generator.tech
"""


from __future__ import absolute_import

import unittest

import openapi_client
from openapi_client.api.daemon_api import DaemonApi  # noqa: E501
from openapi_client.rest import ApiException

@unittest.skip("These won't work until the GET APIs are fixed.")
class TestDaemonApi(unittest.TestCase):
    """DaemonApi unit test stubs"""

    def setUp(self):
        configuration = openapi_client.Configuration()
        configuration.debug = False
        api_client = openapi_client.ApiClient(configuration)
        self.daemon = openapi_client.api.daemon_api.DaemonApi(api_client)
        self.models = openapi_client.models

    def tearDown(self):
        pass

    def test_daemon_shutdown(self):
        self.set_port_in_base_url(8082)
        input_params = self.models.InputParameters(
            self.models.Actions.SHUTDOWN)
        self.daemon.perform_operation(input_params)
        self.set_port_in_base_url()

    def test_set_config(self):
        self.set_port_in_base_url(8083)
        input_params = self.models.InputParameters(
            self.models.Actions.SETCONFIG, "enable_read_range_checksum", "true")
        self.daemon.perform_operation(input_params)
        self.set_port_in_base_url()

    def set_port_in_base_url(self, port=8080):
        self.daemon.api_client.configuration.host = (
                "http://localhost:%d/v1" % port)

if __name__ == '__main__':
    unittest.main()
