// Package upstreamflavor adapts Anthropic Messages API requests to
// non-native deployment targets — Vertex AI, Azure AI Foundry, and AWS
// Bedrock — so a Claude Code-style client can transparently address any
// of them through the lite-proxy.
//
// The native Anthropic API is the proxy's default and does not require a
// flavor adapter; setting UpstreamSelector.AnthropicFlavor to nil
// preserves pre-existing routing behavior byte-for-byte.
//
// Each Flavor owns three concerns:
//
//   - URL construction. Vertex and Bedrock embed the model in the path;
//     Azure uses a deployment URL with an api-version query parameter.
//   - Body transform. Vertex and Bedrock require `anthropic_version`
//     inside the JSON body and reject the top-level `model` field
//     (which has been moved into the URL). Azure leaves the body as
//     Anthropic-native.
//   - Auth injection. Vertex uses OAuth Bearer (GCP access token); Azure
//     uses the `api-key` header; Bedrock uses SigV4 over the entire
//     request (deferred — see bedrock.go).
//
// Flavors are stateless after construction and safe for concurrent use.
package upstreamflavor
