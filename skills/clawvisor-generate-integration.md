Generate a Clawvisor integration YAML file for: $ARGUMENTS

## Instructions

You are generating a Clawvisor integration definition. Follow these steps:

### Step 1: Read the YAML specification

Fetch and read `https://raw.githubusercontent.com/clawvisor/clawvisor/main/docs/INTEGRATION_YAML_SPEC.md`. This is your authoritative reference for the YAML format, field types, and conventions.

### Step 2: Find and read the official API reference

Find the service's published API reference documentation. Do NOT do broad research — go directly to the source:

1. Search for `<service name> API reference` or `<service name> REST API docs`
2. Navigate to the official API reference page (e.g. `developer.example.com/api-reference`)
3. Read the API reference directly — focus on the endpoint listing, auth section, and base URL

Stay on the official documentation site — you may need to read several pages to cover endpoints, auth, and parameters. That's fine. But do NOT:
- Search broadly across the web or do multiple rounds of research
- Read blog posts, tutorials, or third-party guides
- Try to find and download an OpenAPI spec
- Research the company's background or product details

From the API reference, extract:
- Base URL
- Authentication method (OAuth2, API key, Bearer token)
- If OAuth2: authorization URL, token URL, and available scopes
- The 10-30 most practical endpoints (CRUD operations, search, common workflows)

### Step 3: Determine authentication

- If the API uses OAuth2, set up a `pkce_flow` section with:
  - `client_id_env`: `SERVICE_NAME_CLIENT_ID` (SCREAMING_SNAKE_CASE)
  - `scopes`: only the scopes needed by the actions you're generating
  - `authorize_url` and `token_url` from the API docs
- If the API uses API keys or Bearer tokens, use `type: api_key`
- Include `setup_url` pointing to the API key creation page or OAuth app setup page

### Step 4: Generate the YAML

Write the integration YAML following the spec. For each action:
- Use snake_case action names derived from the API operation
- Set risk category and sensitivity honestly (read/search/write/delete, low/medium/high)
- Include only the most useful response fields (max 8 per action)
- Write a summary template that gives useful feedback

### Step 5: Classify risk independently

Review each action and ensure risk classifications follow these rules:
- GET requests are at minimum `read`/`low`
- POST/PUT/PATCH are at minimum `write`/`medium`
- DELETE is always `delete`/`medium` or higher
- Actions touching credentials, permissions, or PII are `high`
- Bulk operations are `high`
- When in doubt, classify higher

### Step 6: Save the file

Write the YAML file to `~/.clawvisor/adapters/<service_id>.yaml`.

Tell the user:
1. The file path where it was saved
2. How many actions were generated
3. What auth setup is needed (e.g. "Set ACME_CLIENT_ID env var" or "Paste your API key when connecting")
4. That they can connect the service on the Clawvisor dashboard Services page
