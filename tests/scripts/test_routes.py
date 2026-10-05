import importlib.util
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location(
    "routes", Path(__file__).resolve().parents[2] / "scripts/routes.py"
)
routes = importlib.util.module_from_spec(spec)
spec.loader.exec_module(routes)


class RouteCatalogTests(unittest.TestCase):
    def test_mounts_registration_queries_and_fallback(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "src/api").mkdir(parents=True)
            (root / "src/main.rs").write_text(
                '.mount([basepath, "/"].concat(), api::web_routes())'
            )
            (root / "src/api/web.rs").write_text("""
                pub fn routes() -> Vec<Route> { routes![alive, item, fallback] }
                #[get("/alive")]
                fn alive() {}
                #[post("/item/<id>?<token>")]
                async fn item() {}
                #[get("/<path..>", rank = 10)] // fallback
                fn fallback() {}
                #[post("/unregistered")]
                fn unused() {}
            """)
            self.assertEqual(routes.catalog(root), "GET /alive\nPOST /item/{id}\n")

    def test_unrecognized_mount_fails_for_review(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "src").mkdir()
            (root / "src/main.rs").write_text(
                '.mount([basepath, "/new"].concat(), api::new_routes())'
            )
            with self.assertRaises(KeyError):
                routes.catalog(root)


if __name__ == "__main__":
    unittest.main()
