import { useEffect, useMemo, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type OrgSSOConfig } from '../api/client'
import { useAuth } from '../hooks/useAuth'

type SSOKind = 'saml' | 'oidc'
type DefaultRole = 'owner' | 'admin' | 'member'

// Placeholder displayed for the OIDC client secret when a value already
// exists server-side. The API never returns the actual secret; if the
// user leaves the field at this sentinel we omit it from the PUT body
// so the existing secret is preserved.
const SECRET_PLACEHOLDER = '••••••••••••'

interface FormState {
  kind: SSOKind
  saml_entity_id: string
  saml_sso_url: string
  saml_certificate_pem: string
  oidc_issuer: string
  oidc_client_id: string
  oidc_client_secret: string
  jit_provision: boolean
  default_role: DefaultRole
  email_domain: string
  enabled: boolean
}

function emptyForm(): FormState {
  return {
    kind: 'saml',
    saml_entity_id: '',
    saml_sso_url: '',
    saml_certificate_pem: '',
    oidc_issuer: '',
    oidc_client_id: '',
    oidc_client_secret: '',
    jit_provision: true,
    default_role: 'member',
    email_domain: '',
    enabled: true,
  }
}

function configToForm(cfg: OrgSSOConfig): FormState {
  return {
    kind: cfg.kind,
    saml_entity_id: cfg.saml_entity_id ?? '',
    saml_sso_url: cfg.saml_sso_url ?? '',
    saml_certificate_pem: cfg.saml_certificate_pem ?? '',
    oidc_issuer: cfg.oidc_issuer ?? '',
    oidc_client_id: cfg.oidc_client_id ?? '',
    // The server returns the secret as empty or omitted (write-only). When
    // we have an existing OIDC config we show the placeholder so the user
    // can tell a value is already set; clearing it explicitly will rotate.
    oidc_client_secret:
      cfg.kind === 'oidc' && (cfg.oidc_client_id ?? '') !== '' ? SECRET_PLACEHOLDER : '',
    jit_provision: cfg.jit_provision,
    default_role: cfg.default_role,
    email_domain: cfg.email_domain ?? '',
    enabled: cfg.enabled,
  }
}

function validate(form: FormState): Record<string, string> {
  const errs: Record<string, string> = {}
  const domain = form.email_domain.trim()
  if (!domain) {
    errs.email_domain = 'Email domain is required.'
  } else if (domain !== domain.toLowerCase()) {
    errs.email_domain = 'Email domain must be lowercase.'
  } else if (domain.includes('@')) {
    errs.email_domain = 'Enter the domain only — no "@".'
  } else if (!domain.includes('.')) {
    errs.email_domain = 'Email domain must contain a dot (e.g. acme.com).'
  }
  if (form.kind === 'saml') {
    if (!form.saml_entity_id.trim()) errs.saml_entity_id = 'IdP entity ID is required.'
    if (!form.saml_sso_url.trim()) {
      errs.saml_sso_url = 'IdP SSO URL is required.'
    } else if (!/^https:\/\//i.test(form.saml_sso_url.trim())) {
      errs.saml_sso_url = 'SSO URL must use https://.'
    }
    const pem = form.saml_certificate_pem.trim()
    if (!pem) {
      errs.saml_certificate_pem = 'Signing certificate is required.'
    } else if (
      !pem.includes('-----BEGIN CERTIFICATE-----') ||
      !pem.includes('-----END CERTIFICATE-----')
    ) {
      errs.saml_certificate_pem = 'Paste the full PEM, including BEGIN/END CERTIFICATE lines.'
    }
  } else {
    if (!form.oidc_issuer.trim()) errs.oidc_issuer = 'OIDC issuer is required.'
    if (!form.oidc_client_id.trim()) errs.oidc_client_id = 'OIDC client ID is required.'
  }
  return errs
}

function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(value)
          setCopied(true)
          setTimeout(() => setCopied(false), 1200)
        } catch {
          /* ignore — clipboard may be unavailable in some contexts */
        }
      }}
      className="text-xs px-2 py-1 rounded border border-border-default hover:bg-surface-0 text-text-secondary"
    >
      {copied ? 'Copied' : 'Copy'}
    </button>
  )
}

function FieldError({ msg }: { msg?: string }) {
  if (!msg) return null
  return <p className="mt-1 text-xs text-danger">{msg}</p>
}

const inputCls =
  'w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary'

export default function OrgSSO() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id ?? ''
  const queryClient = useQueryClient()

  const { data: config, isLoading } = useQuery({
    queryKey: ['sso', orgId],
    queryFn: () => api.orgs.sso.get(orgId),
    enabled: !!orgId,
  })

  const [editing, setEditing] = useState(false)
  const [form, setForm] = useState<FormState>(emptyForm())
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [saveError, setSaveError] = useState<string | null>(null)

  // Sync form with fetched config whenever the editor opens or config changes.
  useEffect(() => {
    if (editing) {
      setForm(config ? configToForm(config) : emptyForm())
      setErrors({})
      setSaveError(null)
    }
  }, [editing, config])

  const saveMutation = useMutation({
    mutationFn: (body: Partial<OrgSSOConfig>) => api.orgs.sso.put(orgId, body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['sso', orgId] })
      setEditing(false)
      setSaveError(null)
    },
    onError: (e: Error) => setSaveError(e.message),
  })

  const deleteMutation = useMutation({
    mutationFn: () => api.orgs.sso.delete(orgId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['sso', orgId] })
      setEditing(false)
    },
  })

  const metadataUrl = useMemo(
    () => `${window.location.origin}/api/auth/sso/saml/metadata?org_id=${orgId}`,
    [orgId],
  )
  const acsUrl = useMemo(() => `${window.location.origin}/api/auth/sso/saml/acs`, [])

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to configure SSO.</p>
  }

  function onSave() {
    const errs = validate(form)
    setErrors(errs)
    if (Object.keys(errs).length > 0) return

    const body: Partial<OrgSSOConfig> = {
      kind: form.kind,
      jit_provision: form.jit_provision,
      default_role: form.default_role,
      email_domain: form.email_domain.trim(),
      enabled: form.enabled,
    }
    if (form.kind === 'saml') {
      body.saml_entity_id = form.saml_entity_id.trim()
      body.saml_sso_url = form.saml_sso_url.trim()
      body.saml_certificate_pem = form.saml_certificate_pem.trim()
    } else {
      body.oidc_issuer = form.oidc_issuer.trim()
      body.oidc_client_id = form.oidc_client_id.trim()
      // Only send the secret when the user actually typed a new value;
      // an unchanged placeholder means "keep the existing secret".
      if (form.oidc_client_secret && form.oidc_client_secret !== SECRET_PLACEHOLDER) {
        body.oidc_client_secret = form.oidc_client_secret
      }
    }
    saveMutation.mutate(body)
  }

  return (
    <div className="p-4 sm:p-8 space-y-10">
      <div>
        <h1 className="text-2xl font-bold text-text-primary mb-2">Single Sign-On</h1>
        <p className="text-sm text-text-secondary mb-6">
          Configure SAML or OIDC SSO for your organization. Users in the configured email
          domain can sign in via your IdP.
        </p>

        {isLoading && <p className="text-sm text-text-secondary">Loading…</p>}

        {!isLoading && !config && !editing && (
          <div className="bg-surface-1 rounded-lg border border-border-default p-6">
            <h3 className="text-sm font-medium text-text-primary mb-2">No SSO configured</h3>
            <p className="text-sm text-text-secondary mb-4">
              Connect your identity provider to let your team sign in with their work
              accounts. We support SAML 2.0 and OpenID Connect.
            </p>
            <button
              onClick={() => setEditing(true)}
              className="px-4 py-2 text-sm font-medium rounded-md bg-brand text-white hover:bg-brand-strong"
            >
              Get started
            </button>
          </div>
        )}

        {!isLoading && config && !editing && (
          <div className="space-y-4">
            <div className="bg-surface-1 rounded-lg border border-border-default p-4">
              <div className="flex items-center justify-between mb-3">
                <div>
                  <div className="text-sm font-medium text-text-primary">
                    {config.kind.toUpperCase()} SSO
                    <span
                      className={
                        'ml-2 text-xs px-1.5 py-0.5 rounded ' +
                        (config.enabled
                          ? 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400'
                          : 'bg-surface-0 text-text-secondary')
                      }
                    >
                      {config.enabled ? 'Enabled' : 'Disabled'}
                    </span>
                  </div>
                  <div className="text-xs text-text-secondary mt-1">
                    Domain: <span className="font-mono">{config.email_domain ?? '—'}</span>
                    {' · '}JIT: {config.jit_provision ? 'on' : 'off'}
                    {' · '}Default role: {config.default_role}
                  </div>
                </div>
                <div className="flex items-center gap-2">
                  <button
                    onClick={() => setEditing(true)}
                    className="px-3 py-1.5 text-xs rounded-md border border-border-default hover:bg-surface-0 text-text-primary"
                  >
                    Edit
                  </button>
                  <button
                    onClick={() => {
                      if (confirm('Delete SSO configuration? Users will no longer be able to sign in via your IdP.')) {
                        deleteMutation.mutate()
                      }
                    }}
                    className="px-3 py-1.5 text-xs rounded-md border border-danger/40 text-danger hover:bg-danger/10"
                  >
                    Delete config
                  </button>
                </div>
              </div>
            </div>

            {config.kind === 'saml' && (
              <div className="bg-surface-1 rounded-lg border border-border-default p-4 space-y-4">
                <div className="flex items-center justify-between gap-2 flex-wrap">
                  <h3 className="text-sm font-medium text-text-primary">SAML service-provider details</h3>
                  <span className="text-xs px-2 py-0.5 rounded bg-brand/10 text-brand font-medium">
                    Required for IdP setup
                  </span>
                </div>

                {config.saml_cert_fingerprint && (
                  <div>
                    <div className="text-xs font-medium text-text-secondary mb-1">
                      Certificate fingerprint
                    </div>
                    <div className="flex items-center gap-2">
                      <code className="flex-1 text-xs font-mono px-2 py-1 rounded bg-surface-0 text-text-primary break-all">
                        {config.saml_cert_fingerprint}
                      </code>
                      <CopyButton value={config.saml_cert_fingerprint} />
                    </div>
                    <p className="mt-1 text-xs text-text-secondary">
                      This is the fingerprint we route assertions by — provide it to your
                      IdP team if they're configuring SP-trust.
                    </p>
                  </div>
                )}

                <div>
                  <div className="text-xs font-medium text-text-secondary mb-1">Metadata URL</div>
                  <div className="flex items-center gap-2">
                    <code className="flex-1 text-xs font-mono px-2 py-1 rounded bg-surface-0 text-text-primary break-all">
                      {metadataUrl}
                    </code>
                    <CopyButton value={metadataUrl} />
                  </div>
                  <p className="mt-1 text-xs text-text-secondary">
                    Provide this URL to your IdP's SP setup.
                  </p>
                </div>

                <div>
                  <div className="text-xs font-medium text-text-secondary mb-1">ACS URL</div>
                  <div className="flex items-center gap-2">
                    <code className="flex-1 text-xs font-mono px-2 py-1 rounded bg-surface-0 text-text-primary break-all">
                      {acsUrl}
                    </code>
                    <CopyButton value={acsUrl} />
                  </div>
                  <p className="mt-1 text-xs text-text-secondary">
                    Your IdP should POST SAMLResponse here.
                  </p>
                </div>
              </div>
            )}

            <div className="bg-surface-1 rounded-lg border border-border-default p-4 space-y-2">
              <h3 className="text-sm font-medium text-text-primary">Verify SSO</h3>
              <p className="text-sm text-text-secondary">
                After saving, sign out and try signing in via the email domain — if the
                assertion routes correctly the page should redirect to{' '}
                <code className="text-xs font-mono">/sso/complete</code>.
              </p>
            </div>
          </div>
        )}

        {editing && (
          <div className="bg-surface-1 rounded-lg border border-border-default p-4 space-y-4">
            <h3 className="text-sm font-medium text-text-primary">
              {config ? 'Edit SSO configuration' : 'New SSO configuration'}
            </h3>

            <div>
              <label className="block text-xs font-medium text-text-secondary mb-1">Kind</label>
              <select
                value={form.kind}
                onChange={(e) => setForm({ ...form, kind: e.target.value as SSOKind })}
                className={inputCls}
              >
                <option value="saml">SAML</option>
                <option value="oidc">OIDC</option>
              </select>
            </div>

            {form.kind === 'saml' && (
              <>
                <div>
                  <label className="block text-xs font-medium text-text-secondary mb-1">
                    IdP Entity ID
                  </label>
                  <input
                    value={form.saml_entity_id}
                    onChange={(e) => setForm({ ...form, saml_entity_id: e.target.value })}
                    placeholder="https://accounts.google.com/o/saml2?idpid=C01abc123"
                    className={inputCls}
                  />
                  <FieldError msg={errors.saml_entity_id} />
                </div>
                <div>
                  <label className="block text-xs font-medium text-text-secondary mb-1">
                    IdP SSO URL (https only)
                  </label>
                  <input
                    value={form.saml_sso_url}
                    onChange={(e) => setForm({ ...form, saml_sso_url: e.target.value })}
                    placeholder="https://idp.example.com/sso"
                    className={inputCls}
                  />
                  <FieldError msg={errors.saml_sso_url} />
                </div>
                <div>
                  <label className="block text-xs font-medium text-text-secondary mb-1">
                    IdP signing certificate (PEM)
                  </label>
                  <textarea
                    value={form.saml_certificate_pem}
                    onChange={(e) => setForm({ ...form, saml_certificate_pem: e.target.value })}
                    rows={8}
                    placeholder={'-----BEGIN CERTIFICATE-----\nMIIC...\n-----END CERTIFICATE-----'}
                    className={inputCls + ' font-mono'}
                  />
                  <p className="mt-1 text-xs text-text-secondary">
                    Paste the IdP's signing certificate including{' '}
                    <code>-----BEGIN CERTIFICATE-----</code> and{' '}
                    <code>-----END CERTIFICATE-----</code>.
                  </p>
                  <FieldError msg={errors.saml_certificate_pem} />
                </div>
              </>
            )}

            {form.kind === 'oidc' && (
              <>
                <div>
                  <label className="block text-xs font-medium text-text-secondary mb-1">
                    OIDC issuer
                  </label>
                  <input
                    value={form.oidc_issuer}
                    onChange={(e) => setForm({ ...form, oidc_issuer: e.target.value })}
                    placeholder="https://login.example.com/"
                    className={inputCls}
                  />
                  <FieldError msg={errors.oidc_issuer} />
                </div>
                <div>
                  <label className="block text-xs font-medium text-text-secondary mb-1">
                    OIDC client ID
                  </label>
                  <input
                    value={form.oidc_client_id}
                    onChange={(e) => setForm({ ...form, oidc_client_id: e.target.value })}
                    className={inputCls}
                  />
                  <FieldError msg={errors.oidc_client_id} />
                </div>
                <div>
                  <label className="block text-xs font-medium text-text-secondary mb-1">
                    Client secret (write-only)
                  </label>
                  <input
                    type="password"
                    value={form.oidc_client_secret}
                    onChange={(e) => setForm({ ...form, oidc_client_secret: e.target.value })}
                    onFocus={(e) => {
                      if (e.target.value === SECRET_PLACEHOLDER) {
                        setForm({ ...form, oidc_client_secret: '' })
                      }
                    }}
                    className={inputCls}
                  />
                  <p className="mt-1 text-xs text-text-secondary">
                    Leave unchanged to keep the existing secret. Enter a new value to rotate.
                  </p>
                </div>
              </>
            )}

            <div>
              <label className="block text-xs font-medium text-text-secondary mb-1">
                Email domain for SSO discovery (e.g. acme.com)
              </label>
              <input
                value={form.email_domain}
                onChange={(e) =>
                  setForm({ ...form, email_domain: e.target.value.toLowerCase() })
                }
                placeholder="acme.com"
                className={inputCls}
              />
              <FieldError msg={errors.email_domain} />
            </div>

            <div className="flex items-start gap-2">
              <input
                id="jit_provision"
                type="checkbox"
                checked={form.jit_provision}
                onChange={(e) => setForm({ ...form, jit_provision: e.target.checked })}
                aria-describedby="jit_provision-help"
                className="mt-1"
              />
              <div>
                <label htmlFor="jit_provision" className="text-sm text-text-primary">
                  JIT provision
                </label>
                <p id="jit_provision-help" className="text-xs text-text-secondary">
                  Create accounts for new users automatically when they sign in via SSO.
                </p>
              </div>
            </div>

            <div>
              <label className="block text-xs font-medium text-text-secondary mb-1">
                Default role for JIT-provisioned users
              </label>
              <select
                value={form.default_role}
                onChange={(e) =>
                  setForm({ ...form, default_role: e.target.value as DefaultRole })
                }
                className={inputCls}
              >
                <option value="member">Member</option>
                <option value="admin">Admin</option>
                <option value="owner">Owner</option>
              </select>
            </div>

            <div className="flex items-start gap-2">
              <input
                id="sso_enabled"
                type="checkbox"
                checked={form.enabled}
                onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
                aria-describedby="sso_enabled-help"
                className="mt-1"
              />
              <div>
                <label htmlFor="sso_enabled" className="text-sm text-text-primary">
                  Enabled
                </label>
                <p id="sso_enabled-help" className="text-xs text-text-secondary">
                  When off, users in this domain fall back to regular login.
                </p>
              </div>
            </div>

            {saveError && (
              <p className="text-xs text-danger">{saveError}</p>
            )}

            <div className="flex items-center gap-2 pt-2">
              <button
                onClick={onSave}
                disabled={saveMutation.isPending}
                className="px-4 py-2 text-sm font-medium rounded-md bg-brand text-white hover:bg-brand-strong disabled:opacity-50"
              >
                {saveMutation.isPending ? 'Saving…' : 'Save'}
              </button>
              <button
                onClick={() => {
                  setEditing(false)
                  setErrors({})
                  setSaveError(null)
                }}
                className="px-4 py-2 text-sm rounded-md border border-border-default hover:bg-surface-0 text-text-primary"
              >
                Cancel
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
