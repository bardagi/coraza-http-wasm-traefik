# Coraza http-wasm traefik plugin

**DISCLAIMER: This project is still in early stages and it can be used to get a grasp on the Coraza + Traefik integration over Wasm. If you are looking for production grade performance I recommend you look into [Coraza native integration with Traefik](https://traefik.io/press/traefik-labs-accelerates-path-towards-api-runtime-governance)**

This repository publishes the coraza-http-wasm as a plugin and also contains examples on how to run coraza-http-wasm as a traefik plugin.

The wasm executable is built in the [coraza-http-wasm](https://github.com/bardagi/coraza-http-wasm/tree/main) repository.

To use the plugin in your own Traefik instance, add it to the static configuration:

```yaml
experimental:
  plugins:
    coraza:
      moduleName: github.com/bardagi/coraza-http-wasm-traefik
      version: v0.4.1
```

## Getting started

You can run the docker compose example:

```console
docker compose up traefik
```

and do test calls:

- `curl -I 'http://localhost:8080/admin'` will return a 403 as per the configuration rules.
- `curl -I 'http://localhost:8080/anything'` will return a 200 as there is not matching rule.

To try out other kind of rules, you can locally modify the `config-dynamic.yaml` file in the section middlewares:

```yaml
http:
# ...
  middlewares:
    waf:
      plugin:
        coraza:
          directives:
            - SecRuleEngine On
            - SecDebugLog /dev/stdout
            - SecDebugLogLevel 9
            - SecRule REQUEST_URI "@streq /admin" "id:101,phase:1,log,deny,status:403"
```

For more information about the available directives go to [coraza docs](https://coraza.io/docs).

## Performance

The example configuration enables `SecDebugLogLevel 9`, which logs every rule evaluation to stdout and is very expensive. In anything beyond local experiments:

- Remove `SecDebugLog`/`SecDebugLogLevel`, or keep the level at 0-3.
- Only enable `SecRequestBodyAccess`/`SecResponseBodyAccess` when you have rules that inspect bodies, and restrict `SecResponseBodyMimeType` to the content types you need, since body inspection requires buffering.
- Set `SecRequestBodyLimit`/`SecResponseBodyLimit` to sensible values for your traffic.
