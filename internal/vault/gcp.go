package vault

import (
	"context"
	"fmt"
	"hash/crc32"
	"strings"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	pkgvault "github.com/clawvisor/clawvisor/pkg/vault"
	"google.golang.org/api/iterator"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GCPVault stores credentials in GCP Secret Manager.
// Credentials are AES-256-GCM encrypted before storing — GCP access control
// is a second layer, not the primary encryption.
// Secret naming: clawvisor-{userID}-{serviceID}
type GCPVault struct {
	client    *secretmanager.Client
	project   string
	localVault *LocalVault // for encryption/decryption
}

// NewGCPVault creates a GCPVault. The masterKey is used for AES-256-GCM encryption
// of credential bytes before they are stored in Secret Manager.
func NewGCPVault(ctx context.Context, project string, masterKey []byte) (*GCPVault, error) {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating Secret Manager client: %w", err)
	}
	lv, err := NewLocalVaultFromKey(masterKey)
	if err != nil {
		return nil, err
	}
	return &GCPVault{client: client, project: project, localVault: lv}, nil
}

// sanitizeSecretID replaces characters not allowed in GCP Secret Manager IDs
// ([a-zA-Z0-9_-]) with underscores.
func sanitizeSecretID(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

func (v *GCPVault) secretName(userID, serviceID string) string {
	safeID := sanitizeSecretID(fmt.Sprintf("clawvisor-%s-%s", userID, serviceID))
	return fmt.Sprintf("projects/%s/secrets/%s", v.project, safeID)
}

func (v *GCPVault) Set(ctx context.Context, userID, serviceID string, credential []byte) error {
	encrypted, iv, authTag, err := v.localVault.encrypt(credential)
	if err != nil {
		return fmt.Errorf("gcp vault encrypt: %w", err)
	}
	// Store as "encrypted|iv|authTag" concatenated with | separator
	payload := []byte(encrypted + "|" + iv + "|" + authTag)

	name := v.secretName(userID, serviceID)
	parent := fmt.Sprintf("projects/%s", v.project)
	secretID := sanitizeSecretID(fmt.Sprintf("clawvisor-%s-%s", userID, serviceID))

	// Ensure secret exists
	_, err = v.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if status.Code(err) == codes.NotFound {
		_, err = v.client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
			Parent:   parent,
			SecretId: secretID,
			Secret: &secretmanagerpb.Secret{
				Replication: &secretmanagerpb.Replication{
					Replication: &secretmanagerpb.Replication_Automatic_{
						Automatic: &secretmanagerpb.Replication_Automatic{},
					},
				},
			},
		})
		if err != nil {
			return fmt.Errorf("creating secret: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("getting secret: %w", err)
	}

	crcVal := int64(crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)))
	_, err = v.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: name,
		Payload: &secretmanagerpb.SecretPayload{
			Data:       payload,
			DataCrc32C: &crcVal,
		},
	})
	return err
}

func (v *GCPVault) Get(ctx context.Context, userID, serviceID string) ([]byte, error) {
	name := v.secretName(userID, serviceID) + "/versions/latest"
	result, err := v.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: name,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, pkgvault.ErrNotFound
		}
		return nil, fmt.Errorf("accessing secret: %w", err)
	}

	payload := string(result.Payload.Data)
	// Parse "encrypted|iv|authTag"
	var encrypted, iv, authTag string
	parts := splitThree(payload)
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid vault payload format")
	}
	encrypted, iv, authTag = parts[0], parts[1], parts[2]

	return v.localVault.decrypt(encrypted, iv, authTag)
}

func (v *GCPVault) Delete(ctx context.Context, userID, serviceID string) error {
	name := v.secretName(userID, serviceID)
	err := v.client.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: name})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

func (v *GCPVault) List(ctx context.Context, userID string) ([]string, error) {
	parent := fmt.Sprintf("projects/%s", v.project)
	prefix := fmt.Sprintf("clawvisor-%s-", userID)

	var services []string
	it := v.client.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: parent})
	for {
		secret, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing secrets: %w", err)
		}
		// Extract secret ID from full name
		// full name: projects/{project}/secrets/{secretID}
		id := secretIDFromName(secret.Name)
		if len(id) > len(prefix) && id[:len(prefix)] == prefix {
			services = append(services, id[len(prefix):])
		}
	}
	return services, nil
}

func splitThree(s string) []string {
	var parts []string
	start := 0
	count := 0
	for i, c := range s {
		if c == '|' && count < 2 {
			parts = append(parts, s[start:i])
			start = i + 1
			count++
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func secretIDFromName(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			return name[i+1:]
		}
	}
	return name
}
