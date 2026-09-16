#!/usr/bin/env python3
"""Explicitly authorized provider checks; secrets are read from the local .env."""
import argparse
import os
from pathlib import Path
import shlex
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--smtp-to', help='Send exactly one SMTP message to this authorized recipient')
parser.add_argument('--chat', action='store_true', help='Spend tokens on two real Anthropic chat turns')
args = parser.parse_args()
if not args.smtp_to and not args.chat:
    parser.error('specify --smtp-to and/or --chat; real services are never enabled implicitly')
root = Path(__file__).resolve().parent.parent
environment = os.environ.copy()
for line in (root / '.env').read_text().splitlines():
    key, separator, value = line.partition('=')
    if not separator or line.lstrip().startswith('#'):
        continue
    # Parse literal values without shell execution or variable expansion.
    parts = shlex.split(value, comments=True)
    environment[key.strip()] = ' '.join(parts)
environment.pop('BROTO_LIVE_SMTP_TO', None)
environment.pop('BROTO_LIVE_CHAT', None)
if args.smtp_to:
    environment['BROTO_LIVE_SMTP_TO'] = args.smtp_to
if args.chat:
    environment['BROTO_LIVE_CHAT'] = 'true'
result = subprocess.run(['sh', 'scripts/test-integration.sh', '-run', '^TestLive', '-v', '-timeout=360s'], cwd=root, env=environment)
raise SystemExit(result.returncode)
