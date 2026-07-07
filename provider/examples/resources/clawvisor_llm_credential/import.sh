# Import an instance-shared key by "<provider>", or an agent-scoped key by
# "<provider>:agent:<agent_id>". api_key / reference cannot be recovered (the
# server is write-only) and must be supplied in config after import.
terraform import clawvisor_llm_credential.anthropic anthropic
terraform import clawvisor_llm_credential.anthropic_ci_agent anthropic:agent:agt_abc123
