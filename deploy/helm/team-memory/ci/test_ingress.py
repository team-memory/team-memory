"""Regression checks for public onboarding ingress routes (requires Helm)."""
import re
import subprocess
import unittest
from pathlib import Path


class IngressTests(unittest.TestCase):
    def test_public_routes(self):
        chart = Path(__file__).resolve().parents[1]
        for enabled, base in [(False, "/portal"), (True, "/portal"), (True, "/")]:
            with self.subTest(portal=enabled, base=base):
                rendered = subprocess.check_output(
                    ["helm", "template", "t", str(chart), "-f", str(chart / "ci/full-values.yaml"), "--show-only",
                     "templates/ingress.yaml", "--set", "ingress.enabled=true",
                     "--set", f"portal.enabled={str(enabled).lower()}",
                     "--set", f"portal.basePath={base}"], text=True,
                )
                routes = re.findall(r"- path: (\S+)\s+pathType: (\S+)", rendered)
                self.assertEqual(len(routes), len(set(path for path, _ in routes)))
                self.assertIn(("/welcome", "Prefix"), routes)
                self.assertIn(("/", "Prefix" if enabled and base == "/" else "Exact"), routes)
                for path in ["/mcp", "/oauth", "/.well-known"]:
                    self.assertIn((path, "Prefix"), routes)
                if base != "/":
                    self.assertEqual((base, "Prefix") in routes, enabled)
                    for private in ["/healthz", "/readyz", "/metrics"]:
                        self.assertFalse(any(
                            private == path or (kind == "Prefix" and private.startswith(path.rstrip("/") + "/"))
                            for path, kind in routes
                        ))


if __name__ == "__main__":
    unittest.main()
