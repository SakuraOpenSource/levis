"""Static and fixture-only installer verification; no root install."""
from pathlib import Path
import unittest

class LevisInstallSecurityTests(unittest.TestCase):
    def test_release_binary_requires_sha256(self):
        script = Path(__file__).with_name('install-levis.sh').read_text(encoding='utf-8')
        self.assertIn('verify_sha256', script)
        self.assertIn('--expected-sha256', script)
        self.assertIn('checksums.txt', script)

if __name__ == '__main__':
    unittest.main()
