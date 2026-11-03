// Package admin owns Jevlar's operator plane: GET /admin/status,
// POST /admin/reload and GET /admin/usage. Admin reads the serving
// snapshot through a caller supplied accessor (internal/app swaps the
// snapshot behind it) and asks a caller supplied function to run the
// reload pipeline; it never imports the proxy package.
//
// # Endpoints
//
// GET /admin/status answers 200 with the serving snapshot's summary:
//
//	{
//	  "version": "dev",                      // binary version
//	  "uptime_seconds": 123,                 // since process boot
//	  "config_hash": "sha256-hex",           // canonical resolved config
//	  "providers": [
//	    {
//	      "name": "typesafe",
//	      "base_url": "https://api.typesafe.ai",
//	      "path": "/v1/systemone",
//	      "auth": "bearer",
//	      "account": "typesafe-main",
//	      "models": ["jev-latest"],
//	      "timeout_ms": 8000,
//	      "max_concurrency": 64,
//	      "key_count": 2,
//	      "key_ids": ["primary", "secondary"]
//	    }
//	  ],
//	  "models": {                            // alias -> ordered targets
//	    "jev-latest": [{"provider": "typesafe", "model": "jev-latest"}]
//	  },
//	  "auth": {"key_ids": ["app-prod", "batch"]},
//	  "breaker": {"threshold": 5, "cooldown_ms": 30000}
//	}
//
// Providers are listed in configuration order. Key material never
// appears: no key values, no inbound hashes — only ids and counts.
//
// POST /admin/reload runs the reload pipeline (load, validate, build,
// swap). Success answers 200 with {"status":"ok","config_hash":"..."}.
// Failure answers 500 with {"status":"error","errors":[{"field":"...",
// "message":"..."}]} (one entry per configuration violation) and the
// previous runtime keeps serving.
//
// GET /admin/usage answers totals rolled up by client, provider, key and
// model. With ?window=<duration> (maximum 90 days) it sums the durable
// rollups instead of the in-memory totals since boot; that mode needs
// usage.enabled = true.
//
// # Access
//
// When the configuration sets server.admin_token, every request must
// carry "Authorization: Bearer <token>"; both sides are SHA-256 hashed and
// compared in constant time, so the token length does not leak through
// timing. When no token is set the admin listener is required to be
// loopback (a configuration rule). The guard independently rejects a
// non-loopback remote address, a non-loopback Host header (DNS rebinding)
// and a browser request marked cross-site (Sec-Fetch-Site/Origin), so a
// web page cannot drive POST /admin/reload.
package admin
