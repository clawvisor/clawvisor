export type ServiceBasePolicyBlock = {
  action: string
  reason: string
}

export type ServiceBasePolicy = {
  serviceId: string
  name: string
  description: string
  blocks: ServiceBasePolicyBlock[]
}

/** Safe defaults applied when a Gmail account is first connected. */
export const GMAIL_BASE_POLICY: ServiceBasePolicy = {
  serviceId: 'google.gmail',
  name: 'Gmail',
  description:
    'Read and search mail is allowed. Outbound send is blocked until you explicitly allow it in Policy.',
  blocks: [
    {
      action: 'send_message',
      reason: 'Base policy: outbound email blocked by default',
    },
  ],
}

const BASE_POLICIES: Record<string, ServiceBasePolicy> = {
  'google.gmail': GMAIL_BASE_POLICY,
}

export function getServiceBasePolicy(serviceId: string): ServiceBasePolicy | undefined {
  const base = serviceId.includes(':') ? serviceId.slice(0, serviceId.indexOf(':')) : serviceId
  return BASE_POLICIES[base]
}

export function serviceKey(svc: { id: string; alias?: string }): string {
  return svc.alias ? `${svc.id}:${svc.alias}` : svc.id
}

export function basePolicyApplied(
  svcKey: string,
  policy: ServiceBasePolicy,
  restrictions: Array<{ service: string; action: string }>,
): boolean {
  return policy.blocks.every(block =>
    restrictions.some(r => r.service === svcKey && r.action === block.action),
  )
}

export function missingBasePolicyBlocks(
  svcKey: string,
  policy: ServiceBasePolicy,
  restrictions: Array<{ service: string; action: string }>,
): ServiceBasePolicyBlock[] {
  return policy.blocks.filter(
    block => !restrictions.some(r => r.service === svcKey && r.action === block.action),
  )
}
