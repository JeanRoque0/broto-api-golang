import unittest
from unittest.mock import patch
import deploy

class DeployTests(unittest.TestCase):
    def test_register_drops_readonly_fields_and_keeps_security(self):
        original = {'family': 'api', 'revision': 7, 'status': 'ACTIVE', 'taskDefinitionArn': 'old',
                    'containerDefinitions': [{'name': 'api', 'image': 'old', 'readonlyRootFilesystem': True}]}
        result = deploy.definition(original, 'new@sha256:abc', 'api')
        self.assertNotIn('revision', result)
        self.assertTrue(result['containerDefinitions'][0]['readonlyRootFilesystem'])
        self.assertEqual(original['containerDefinitions'][0]['image'], 'old')
        with self.assertRaises(ValueError): deploy.definition(original, 'new', 'wrong')

    def test_rollback_is_not_success(self):
        current = {'deployments': [{'status': 'PRIMARY', 'taskDefinition': 'old', 'rolloutState': 'COMPLETED'}],
                   'runningCount': 1, 'desiredCount': 1, 'pendingCount': 0}
        with self.assertRaises(RuntimeError): deploy.deployment_status(current, 'new')
        self.assertTrue(deploy.deployment_status(current, 'old'))
        current['pendingCount'] = 1
        self.assertFalse(deploy.deployment_status(current, 'old'))

    def test_existing_password_is_not_rotated(self):
        with patch.object(deploy, 'aws', return_value={'SecretString': '{"password":"' + 'x'*48 + '"}'}) as client:
            deploy.ensure_password('secret')
            self.assertEqual(client.call_count, 1)

    def test_denied_secret_is_not_overwritten(self):
        with patch.object(deploy, 'aws', side_effect=RuntimeError('AccessDeniedException')) as client:
            with self.assertRaises(RuntimeError): deploy.ensure_password('secret')
            self.assertEqual(client.call_count, 1)

if __name__ == '__main__': unittest.main()
