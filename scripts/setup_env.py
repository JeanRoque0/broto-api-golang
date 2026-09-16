"""Create a local .env once, without printing secrets or replacing existing values."""
from pathlib import Path
import secrets
root = Path(__file__).resolve().parents[1]
text = (root / '.env.example').read_text()
text = text.replace('POSTGRES_PASSWORD=\n', 'POSTGRES_PASSWORD=' + secrets.token_hex(24) + '\n')
text = text.replace('SIGNING_KEY=\n', 'SIGNING_KEY=' + secrets.token_hex(32) + '\n')
with (root / '.env').open('x') as f:
    f.write(text)
(root / '.env').chmod(0o600)
print('Created local .env. Development email auto-confirmation is enabled.')
