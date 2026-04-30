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

(function () {
  try {
    const proxyURL =
      process.env.HTTPS_PROXY ||
      process.env.HTTP_PROXY ||
      process.env.https_proxy ||
      process.env.http_proxy;
    if (!proxyURL) return;

    if (globalThis.__clawvisorProxyShimInstalled) return;

    let undici;
    try {
      const { createRequire } = require("module");
      const cwdRequire = createRequire(process.cwd() + "/.");
      undici = cwdRequire("undici");
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
        // Continue without CA injection; TLS will fail loudly if needed.
      }
    }

    let parsed;
    try {
      parsed = new URL(proxyURL);
    } catch (_) {
      return;
    }
    const opts = {
      uri: parsed.protocol + "//" + parsed.host + parsed.pathname,
    };
    if (parsed.username || parsed.password) {
      const user = decodeURIComponent(parsed.username || "");
      const pass = decodeURIComponent(parsed.password || "");
      opts.token =
        "Basic " + Buffer.from(user + ":" + pass).toString("base64");
    }
    if (ca) opts.requestTls = { ca };

    undici.setGlobalDispatcher(new undici.ProxyAgent(opts));

    const wrap = (Ctor, isEnvAgent) => {
      const Wrapped = function (cfg) {
        let cfgObj = cfg;
        if (typeof cfg === "string") cfgObj = { uri: cfg };
        if (!cfgObj || typeof cfgObj !== "object") cfgObj = {};

        if (opts.token && !cfgObj.token) cfgObj.token = opts.token;
        if (opts.requestTls && !cfgObj.requestTls) {
          cfgObj.requestTls = opts.requestTls;
        }
        if (!isEnvAgent && opts.uri && !cfgObj.uri) cfgObj.uri = opts.uri;
        if (cfgObj.uri) {
          try {
            const u = new URL(cfgObj.uri);
            if (u.username || u.password) {
              cfgObj.uri = u.protocol + "//" + u.host + u.pathname;
            }
          } catch (_) {}
        }
        return new Ctor(cfgObj);
      };
      Wrapped.prototype = Ctor.prototype;
      return Wrapped;
    };
    if (typeof undici.ProxyAgent === "function") {
      undici.ProxyAgent = wrap(undici.ProxyAgent, false);
    }
    if (typeof undici.EnvHttpProxyAgent === "function") {
      undici.EnvHttpProxyAgent = wrap(undici.EnvHttpProxyAgent, true);
    }

    globalThis.__clawvisorProxyShimInstalled = true;
  } catch (_) {
    // Never prevent the child process from starting.
  }
})();
