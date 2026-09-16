#!/usr/bin/env python3
"""Deploy immutable images; secrets stay out of arguments, logs and Terraform state."""
import json
import os
import re
import subprocess
import tempfile
import time


def aws(*args):
    result = subprocess.run(['aws', *args, '--output', 'json', '--no-cli-pager'], capture_output=True, text=True)
    if result.returncode:
        # Do not propagate responses that may contain application secrets.
        code = re.search(r'\(([A-Za-z0-9]+)\)', result.stderr)
        raise RuntimeError(code.group(1) if code else 'AWS command failed: ' + ' '.join(args[:2]))
    return json.loads(result.stdout or '{}')


def json_arg(value, callback):
    with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as file:
        json.dump(value, file)
        file.flush()
        return callback('file://' + file.name)


def ensure_password(arn):
    try:
        value = json.loads(aws('secretsmanager', 'get-secret-value', '--secret-id', arn)['SecretString'])
        if len(value.get('password', '')) < 32:
            raise ValueError('Existing application DB password must contain at least 32 characters')
    except RuntimeError as error:
        if str(error) != 'ResourceNotFoundException':
            raise
        password = aws('secretsmanager', 'get-random-password', '--password-length', '48', '--exclude-punctuation')['RandomPassword']
        json_arg({'password': password}, lambda path: aws('secretsmanager', 'put-secret-value', '--secret-id', arn, '--secret-string', path))


FIELDS = ('family taskRoleArn executionRoleArn networkMode containerDefinitions volumes placementConstraints '
          'requiresCompatibilities cpu memory pidMode ipcMode proxyConfiguration inferenceAccelerators '
          'ephemeralStorage runtimePlatform enableFaultInjection').split()


def definition(source, image, container):
    value = {k: source[k] for k in FIELDS if k in source}
    # JSON clone avoids mutating a caller's baseline.
    value = json.loads(json.dumps(value))
    targets = [c for c in value['containerDefinitions'] if c['name'] == container]
    if len(targets) != 1:
        raise ValueError('Expected exactly one application container')
    targets[0]['image'] = image
    return value


def register(family, image, container):
    source = aws('ecs', 'describe-task-definition', '--task-definition', family)['taskDefinition']
    value = definition(source, image, container)
    return json_arg(value, lambda path: aws('ecs', 'register-task-definition', '--cli-input-json', path))['taskDefinition']['taskDefinitionArn']


def service(cluster, name):
    response = aws('ecs', 'describe-services', '--cluster', cluster, '--services', name)
    if response.get('failures') or len(response.get('services', [])) != 1:
        raise RuntimeError('ECS service not found')
    return response['services'][0]


def deployment_status(current, expected):
    primary = next((d for d in current.get('deployments', []) if d['status'] == 'PRIMARY'), None)
    if not primary:
        return False
    if primary.get('rolloutState') == 'FAILED':
        raise RuntimeError('ECS rollout failed')
    if primary['taskDefinition'] != expected:
        raise RuntimeError('ECS rolled back or another deployment replaced this release')
    return (primary.get('rolloutState') == 'COMPLETED' and len(current['deployments']) == 1
            and current['runningCount'] == current['desiredCount'] and current['pendingCount'] == 0)


def main():
    names = ['IMAGE_URI', 'ECS_CLUSTER', 'ECS_SERVICE', 'ECS_TASK_FAMILY', 'ECS_MIGRATION_FAMILY',
             'ECS_CAPACITY_PROVIDER', 'DB_APP_SECRET_ARN', 'ECS_MIN_TASKS']
    for key in names:
        if not os.environ.get(key):
            raise ValueError('Missing environment variable: ' + key)
    image = os.environ['IMAGE_URI']
    if not re.fullmatch(r'[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/[a-z0-9/_-]+@sha256:[a-f0-9]{64}', image):
        raise ValueError('Deploy requires an immutable ECR image digest')
    cluster, name = os.environ['ECS_CLUSTER'], os.environ['ECS_SERVICE']
    ensure_password(os.environ['DB_APP_SECRET_ARN'])
    migration = register(os.environ['ECS_MIGRATION_FAMILY'], image, 'migrate')
    result = aws('ecs', 'run-task', '--cluster', cluster, '--task-definition', migration,
                 '--capacity-provider-strategy', 'capacityProvider=' + os.environ['ECS_CAPACITY_PROVIDER'] + ',weight=1', '--count', '1')
    if result.get('failures') or len(result.get('tasks', [])) != 1:
        raise RuntimeError('Migration task could not be placed')
    task = result['tasks'][0]['taskArn']
    for _ in range(120):
        result = aws('ecs', 'describe-tasks', '--cluster', cluster, '--tasks', task)
        if result.get('failures') or not result.get('tasks'):
            raise RuntimeError('Migration task disappeared')
        current = result['tasks'][0]
        if current['lastStatus'] == 'STOPPED':
            containers = current.get('containers', [])
            if len(containers) != 1 or containers[0].get('exitCode') != 0:
                raise RuntimeError('Migration failed; inspect the migration CloudWatch log')
            break
        time.sleep(10)
    else:
        raise RuntimeError('Migration timeout; inspect task before retrying')
    app = register(os.environ['ECS_TASK_FAMILY'], image, 'api')
    count = max(service(cluster, name)['desiredCount'], int(os.environ['ECS_MIN_TASKS']))
    aws('ecs', 'update-service', '--cluster', cluster, '--service', name, '--task-definition', app, '--desired-count', str(count))
    for _ in range(150):
        if deployment_status(service(cluster, name), app):
            print('Deployment healthy: ' + app)
            return
        time.sleep(10)
    raise RuntimeError('Deployment timeout; inspect ECS before retrying')


if __name__ == '__main__':
    main()
