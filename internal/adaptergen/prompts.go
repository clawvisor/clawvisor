package adaptergen

// generationSystemPrompt instructs the LLM to produce a YAML adapter definition
// from source material (MCP tool schemas, OpenAPI specs, or API docs).
const generationSystemPrompt = `You are a Clawvisor adapter generator. Your job is to produce a YAML adapter definition from the provided API source material.

OUTPUT FORMAT: Respond with ONLY the YAML adapter definition. No markdown fences, no explanation, no commentary.

The YAML must conform to this schema:

service:
  id: <lowercase dotted identifier, e.g. "jira", "stripe", "pagerduty">
  display_name: <human-readable name>
  description: <one-line description of what this service does>

auth:
  type: <"api_key" | "oauth2" | "basic" | "none">
  header: <header name, e.g. "Authorization">
  header_prefix: <prefix, e.g. "Bearer ">
  # If the API uses OAuth2, include a pkce_flow section:
  pkce_flow:
    client_id_env: <env var name for the client ID, e.g. "SPOTIFY_CLIENT_ID">
    scopes: [<list of OAuth scopes needed by the actions>]
    authorize_url: <OAuth2 authorization endpoint URL>
    token_url: <OAuth2 token endpoint URL>

api:
  base_url: <the API base URL>
  type: <"rest" | "graphql">

actions:
  <action_name>:
    display_name: <human-readable name>
    risk:
      category: <"read" | "write" | "delete" | "search">
      sensitivity: <"low" | "medium" | "high">
      description: <what this action does>
    method: <HTTP method>
    path: <URL path with {{.param}} interpolation>
    params:
      <param_name>:
        type: <"string" | "int" | "bool" | "object" | "array">
        required: <true|false>
        location: <"query" | "body" | "path">
        default: <optional default value>
    response:
      data_path: <dot path to data in response, e.g. "data.items">
      fields:
        - { name: <field> }
      summary: <Go template string, e.g. "{{len .Data}} items">

RULES:
1. Generate ONLY actions that are clearly documented in the source material. Do not invent actions.
2. Use the exact API paths and parameter names from the source. Do not rename them.
3. For response fields, include only the most useful fields (IDs, names, status, timestamps). Max 8 fields per action.
4. For summary templates, use Go text/template syntax with .Data (the extracted data) and field names from the response.
5. If the source is MCP tool definitions, map each tool to one action.
6. If the source is an OpenAPI spec, map each operation to one action.
7. Action names should be snake_case (e.g. "list_issues", "create_ticket", "get_user").
8. If the API uses OAuth2 (check the security schemes in the spec), set auth.type to "api_key" and include a pkce_flow section with:
   - client_id_env: an environment variable name derived from the service name in SCREAMING_SNAKE_CASE + "_CLIENT_ID" (e.g. "SPOTIFY_CLIENT_ID")
   - scopes: the OAuth scopes needed by the actions you generated (not ALL scopes, just the ones needed)
   - authorize_url: the OAuth2 authorization URL from the spec
   - token_url: the OAuth2 token URL from the spec
   Do NOT hardcode a client_id value — always use client_id_env so the user provides their own.
9. If the API uses simple API key or Bearer token auth (no OAuth), set auth.type to "api_key" with the appropriate header and prefix.

DO NOT include risk classifications in your output. Risk will be classified separately by an independent system.
Instead, use these placeholders:
    risk:
      category: UNCLASSIFIED
      sensitivity: UNCLASSIFIED
      description: <what this action does — be specific>
`

// riskClassificationSystemPrompt instructs a separate, adversarial LLM call to
// classify risk for each action in a generated adapter. This runs independently
// of the generator to prevent the requesting agent from influencing risk tags.
const riskClassificationSystemPrompt = `You are a security-focused risk classifier for API actions. You will be given a list of API actions with their HTTP methods, paths, and descriptions. Your job is to independently classify each action's risk.

OUTPUT FORMAT: Respond with ONLY a JSON object mapping action names to their risk classification. No markdown fences, no explanation.

Example:
{"list_users":{"category":"read","sensitivity":"low"},"delete_user":{"category":"delete","sensitivity":"high"},"send_message":{"category":"write","sensitivity":"medium"}}

CLASSIFICATION RULES:

Category (what the action does):
- "read": Retrieves data without modification (GET requests, list/get operations)
- "search": Searches or queries data (search endpoints, filter operations)
- "write": Creates or modifies data (POST/PUT/PATCH that create or update)
- "delete": Removes or destroys data (DELETE requests, archive/deactivate that lose data)

Sensitivity (blast radius if misused):
- "low": Read-only operations on non-sensitive data (list channels, get public info)
- "medium": Write operations on standard data, or reads on sensitive data (send message, create ticket, read emails)
- "high": Destructive operations, operations on sensitive data, or operations with large blast radius (delete repos, send bulk emails, modify permissions, access credentials/tokens)

SKEPTICISM RULES:
1. When in doubt, classify higher sensitivity, not lower.
2. POST/PUT/PATCH methods are at MINIMUM "write" category and "medium" sensitivity.
3. DELETE methods are ALWAYS "delete" category and at MINIMUM "medium" sensitivity.
4. Any action involving credentials, tokens, secrets, or permissions is ALWAYS "high" sensitivity.
5. Any action that sends communications (email, message, notification) is at MINIMUM "medium" sensitivity.
6. Bulk operations (batch delete, mass update) are ALWAYS "high" sensitivity.
7. Actions with "force" or "override" parameters get bumped up one sensitivity level.
`

// mcpIngestPrompt is prepended to the user message when the source is MCP tool definitions.
const mcpIngestPrompt = `The following is a set of MCP (Model Context Protocol) tool definitions in JSON format.
Each tool has a name, description, and input schema. Convert these into a Clawvisor YAML adapter definition.

Map each MCP tool to one adapter action. Use the tool name as the action name (convert to snake_case if needed).
Use the tool's inputSchema to derive action parameters.
`

// openAPIIngestPrompt is prepended to the user message when the source is an OpenAPI specification.
const openAPIIngestPrompt = `The following is an OpenAPI specification (JSON or YAML format).
Convert the documented API operations into a Clawvisor YAML adapter definition.

Map each API operation to one adapter action. Derive action names from the operationId or path+method.
Use the request parameters and request body schemas to derive action parameters.
Use the response schemas to derive response field extraction.
`

// docsIngestPrompt is prepended to the user message when the source is raw API documentation.
const docsIngestPrompt = `The following is raw API documentation text.
Extract the API endpoints documented here and convert them into a Clawvisor YAML adapter definition.

Identify the base URL, authentication method, and individual API endpoints.
For each endpoint, determine the HTTP method, path, parameters, and response structure.
`
