// Clawvisor proxy shim — preloaded into Node child processes via
// NODE_OPTIONS=--require so that built-in fetch() (undici) routes
// through the proxy. Node's http module honors HTTP_PROXY natively;
// undici does not, which is why this exists.
//
// Failure modes are intentionally silent: if undici can't be required,
// or the env vars aren't set, we no-op so wrapped commands that don't
// use fetch() are unaffected. Worst case the child bypasses the proxy
// (same as today); we never crash a Node process the user is trying
// to run.
//
// Materialized to ~/.clawvisor/local/clawvisor-proxy-shim.js by the
// wrapper. Don't edit the file in place — edits get overwritten.

(function () {
  try {
    const proxyURL =
      process.env.HTTPS_PROXY ||
      process.env.HTTP_PROXY ||
      process.env.https_proxy ||
      process.env.http_proxy;
    if (!proxyURL) return;

    // Idempotent — multiple --require invocations of this file in the
    // same process (which can happen when a wrapped command spawns
    // node and the env propagates) shouldn't reset the dispatcher.
    if (globalThis.__clawvisorProxyShimInstalled) return;

    let undici;
    try {
      // Resolve from the wrapped project's node_modules. Most modern
      // Node projects have undici directly or transitively; if not,
      // this throws and we fall through silently.
      undici = require("undici");
    } catch (_) {
      return;
    }
    if (!undici || typeof undici.setGlobalDispatcher !== "function" || typeof undici.ProxyAgent !== "function") {
      return;
    }

    let ca;
    const caPath = process.env.NODE_EXTRA_CA_CERTS || process.env.CLAWVISOR_PROXY_CA;
    if (caPath) {
      try {
        ca = require("fs").readFileSync(caPath);
      } catch (_) {
        // Cert read failed; proceed without — TLS verification will
        // fail loudly, which is the right signal.
      }
    }

    const opts = { uri: proxyURL };
    if (ca) opts.requestTls = { ca };

    undici.setGlobalDispatcher(new undici.ProxyAgent(opts));
    globalThis.__clawvisorProxyShimInstalled = true;
  } catch (_) {
    // Belt-and-suspenders. Anything inside this preload that throws
    // would prevent the child from starting; swallow to preserve
    // child invariants.
  }
})();
