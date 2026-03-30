// Shared service/action display utilities. Zero React dependencies.

export const SERVICE_DISPLAY_NAMES: Record<string, string> = {
  'google.gmail': 'Gmail',
  'google.calendar': 'Google Calendar',
  'google.drive': 'Google Drive',
  'google.contacts': 'Google Contacts',
  'github': 'GitHub',
  'apple.imessage': 'iMessage',
  'slack': 'Slack',
  'notion': 'Notion',
  'linear': 'Linear',
  'stripe': 'Stripe',
  'twilio': 'Twilio',
}

export const SERVICE_BRAND_COLORS: Record<string, {
  bg: string
  bgLight: string
  text: string
  border: string
  dot: string
}> = {
  'google.gmail': {
    bg: 'bg-red-600',
    bgLight: 'bg-red-500/10',
    text: 'text-red-400',
    border: 'border-red-400',
    dot: 'bg-red-500',
  },
  'google.calendar': {
    bg: 'bg-blue-600',
    bgLight: 'bg-blue-500/10',
    text: 'text-blue-400',
    border: 'border-blue-400',
    dot: 'bg-blue-500',
  },
  'google.drive': {
    bg: 'bg-yellow-500',
    bgLight: 'bg-yellow-500/10',
    text: 'text-yellow-400',
    border: 'border-yellow-400',
    dot: 'bg-yellow-500',
  },
  'google.contacts': {
    bg: 'bg-sky-600',
    bgLight: 'bg-sky-500/10',
    text: 'text-sky-400',
    border: 'border-sky-400',
    dot: 'bg-sky-500',
  },
  'github': {
    bg: 'bg-gray-800',
    bgLight: 'bg-gray-500/10',
    text: 'text-gray-400',
    border: 'border-gray-600',
    dot: 'bg-gray-700',
  },
  'apple.imessage': {
    bg: 'bg-green-600',
    bgLight: 'bg-green-500/10',
    text: 'text-green-400',
    border: 'border-green-500',
    dot: 'bg-green-500',
  },
  'slack': {
    bg: 'bg-purple-600',
    bgLight: 'bg-purple-500/10',
    text: 'text-purple-400',
    border: 'border-purple-400',
    dot: 'bg-purple-500',
  },
  'notion': {
    bg: 'bg-neutral-800',
    bgLight: 'bg-neutral-500/10',
    text: 'text-neutral-400',
    border: 'border-neutral-600',
    dot: 'bg-neutral-700',
  },
  'linear': {
    bg: 'bg-violet-600',
    bgLight: 'bg-violet-500/10',
    text: 'text-violet-400',
    border: 'border-violet-400',
    dot: 'bg-violet-500',
  },
  'stripe': {
    bg: 'bg-indigo-600',
    bgLight: 'bg-indigo-500/10',
    text: 'text-indigo-400',
    border: 'border-indigo-400',
    dot: 'bg-indigo-500',
  },
  'twilio': {
    bg: 'bg-red-600',
    bgLight: 'bg-red-500/10',
    text: 'text-red-400',
    border: 'border-red-400',
    dot: 'bg-red-500',
  },
}

export const SERVICE_DESCRIPTIONS: Record<string, string> = {
  'google.gmail': 'Read, search, send, and draft email',
  'google.calendar': 'View and manage calendar events',
  'google.drive': 'List, search, and manage files',
  'google.contacts': 'Search and view contacts',
  'github': 'Issues, PRs, and code review',
  'apple.imessage': 'Search and read iMessage threads',
  'slack': 'Channels, messages, and search',
  'notion': 'Pages, databases, and search',
  'linear': 'Issues, projects, and teams',
  'stripe': 'Customers, charges, and subscriptions',
  'twilio': 'SMS, WhatsApp, and messaging',
}

export function serviceDescription(id: string): string {
  const base = id.includes(':') ? id.slice(0, id.indexOf(':')) : id
  return SERVICE_DESCRIPTIONS[base] ?? ''
}

export const ACTION_DISPLAY_NAMES: Record<string, string> = {
  // Gmail
  list_messages: 'List messages',
  get_message: 'Get message',
  send_message: 'Send message',
  create_draft: 'Create draft',
  // Calendar
  list_events: 'List events',
  get_event: 'Get event',
  create_event: 'Create event',
  update_event: 'Update event',
  delete_event: 'Delete event',
  list_calendars: 'List calendars',
  // Drive
  list_files: 'List files',
  get_file: 'Get file',
  create_file: 'Create file',
  update_file: 'Update file',
  search_files: 'Search files',
  // Contacts
  list_contacts: 'List contacts',
  get_contact: 'Get contact',
  search_contacts: 'Search contacts',
  // GitHub
  list_issues: 'List issues',
  get_issue: 'Get issue',
  create_issue: 'Create issue',
  comment_issue: 'Comment on issue',
  list_prs: 'List PRs',
  get_pr: 'Get PR',
  list_repos: 'List repos',
  search_code: 'Search code',
  // iMessage
  search_messages: 'Search messages',
  list_threads: 'List threads',
  get_thread: 'Get thread',
  // send_message already covered by Gmail — same key, same label
  // Slack
  list_channels: 'List channels',
  get_channel: 'Get channel',
  // list_messages, send_message, search_messages already covered
  list_users: 'List users',
  // Notion
  search: 'Search',
  get_page: 'Get page',
  create_page: 'Create page',
  update_page: 'Update page',
  query_database: 'Query database',
  list_databases: 'List databases',
  // Linear
  // list_issues, get_issue, create_issue already covered
  update_issue: 'Update issue',
  add_comment: 'Add comment',
  list_teams: 'List teams',
  list_projects: 'List projects',
  search_issues: 'Search issues',
  // Stripe
  list_customers: 'List customers',
  get_customer: 'Get customer',
  list_charges: 'List charges',
  get_charge: 'Get charge',
  list_subscriptions: 'List subscriptions',
  get_subscription: 'Get subscription',
  create_refund: 'Create refund',
  get_balance: 'Get balance',
  // Twilio
  send_sms: 'Send SMS',
  send_whatsapp: 'Send WhatsApp',
  // list_messages, get_message already covered
}

const DEFAULT_BRAND = {
  bg: 'bg-gray-600',
  bgLight: 'bg-gray-500/10',
  text: 'text-gray-400',
  border: 'border-gray-600',
  dot: 'bg-gray-400',
}

export function serviceName(id: string, alias?: string): string {
  // Handle alias: "google.gmail:personal" → "Gmail (personal)"
  const colonIdx = id.indexOf(':')
  if (colonIdx >= 0) {
    const base = id.slice(0, colonIdx)
    const a = id.slice(colonIdx + 1)
    const name = SERVICE_DISPLAY_NAMES[base] ?? base
    return `${name} (${a})`
  }
  const name = SERVICE_DISPLAY_NAMES[id] ?? id
  if (alias && alias !== 'default') {
    return `${name} (${alias})`
  }
  return name
}

export function serviceBrand(id: string) {
  // Strip alias suffix for brand lookup: "google.gmail:personal" → "google.gmail"
  const base = id.includes(':') ? id.slice(0, id.indexOf(':')) : id
  return SERVICE_BRAND_COLORS[base] ?? DEFAULT_BRAND
}

export function actionName(action: string): string {
  if (action === '*') return 'All actions'
  return ACTION_DISPLAY_NAMES[action] ?? action.replace(/_/g, ' ')
}

export function formatServiceAction(service: string, action: string): string {
  return `${serviceName(service)}: ${actionName(action)}`
}
