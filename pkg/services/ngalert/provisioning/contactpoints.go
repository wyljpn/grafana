package provisioning

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/grafana/grafana/pkg/infra/log"
	apimodels "github.com/grafana/grafana/pkg/services/ngalert/api/tooling/definitions"
	"github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
	"github.com/grafana/grafana/pkg/services/secrets"
	"github.com/grafana/grafana/pkg/util"
	"github.com/prometheus/alertmanager/config"
)

type EmbeddedContactPointService struct {
	amStore           store.AlertingStore
	encryptionService secrets.Service
	provenanceStore   ProvisioningStore
	xact              TransactionManager
	log               log.Logger
}

func NewEmbeddedContactPointService(store store.AlertingStore, encryptionService secrets.Service,
	provenanceStore ProvisioningStore, xact TransactionManager, log log.Logger) *EmbeddedContactPointService {
	return &EmbeddedContactPointService{
		amStore:           store,
		encryptionService: encryptionService,
		provenanceStore:   provenanceStore,
		xact:              xact,
		log:               log,
	}
}

func (ecp *EmbeddedContactPointService) GetContactPoints(ctx context.Context, orgID int64) ([]apimodels.EmbeddedContactPoint, error) {
	cfg, _, err := ecp.getCurrentConfig(ctx, orgID)
	if err != nil {
		return nil, err
	}
	provenances, err := ecp.provenanceStore.GetProvenances(ctx, orgID, "contactpoint")
	if err != nil {
		return nil, err
	}
	ecp.log.Info("provenances", "map", provenances)
	contactPoints := []apimodels.EmbeddedContactPoint{}
	for _, contactPoint := range cfg.GetGrafanaReceiverMap() {
		embeddedContactPoint := apimodels.EmbeddedContactPoint{
			UID:                   contactPoint.UID,
			Type:                  contactPoint.Type,
			Name:                  contactPoint.Name,
			DisableResolveMessage: contactPoint.DisableResolveMessage,
			Settings:              contactPoint.Settings,
		}
		if val, exists := provenances[embeddedContactPoint.UID]; exists && val != "" {
			embeddedContactPoint.Provenance = string(val)
		}
		for k, v := range contactPoint.SecureSettings {
			decryptedValue, err := ecp.decrypteValue(v)
			if err != nil {
				// TODO(JP): log a warning
				continue
			}
			if decryptedValue == "" {
				continue
			}
			embeddedContactPoint.Settings.Set(k, apimodels.RedactedValue)
		}
		contactPoints = append(contactPoints, embeddedContactPoint)
	}
	return contactPoints, nil
}

// internal only
func (ecp *EmbeddedContactPointService) getContactPointUncrypted(ctx context.Context, orgID int64, uid string) (apimodels.EmbeddedContactPoint, error) {
	cfg, _, err := ecp.getCurrentConfig(ctx, orgID)
	if err != nil {
		return apimodels.EmbeddedContactPoint{}, err
	}
	for _, receiver := range cfg.GetGrafanaReceiverMap() {
		if receiver.UID != uid {
			continue
		}
		embeddedContactPoint := apimodels.EmbeddedContactPoint{
			UID:                   receiver.UID,
			Type:                  receiver.Type,
			Name:                  receiver.Name,
			DisableResolveMessage: receiver.DisableResolveMessage,
			Settings:              receiver.Settings,
		}
		for k, v := range receiver.SecureSettings {
			decryptedValue, err := ecp.decrypteValue(v)
			if err != nil {
				// TODO(JP): log a warning
				continue
			}
			if decryptedValue == "" {
				continue
			}
			embeddedContactPoint.Settings.Set(k, decryptedValue)

		}
		return embeddedContactPoint, nil
	}
	return apimodels.EmbeddedContactPoint{}, fmt.Errorf("contact point with uid '%s' not found", uid)
}

func (ecp *EmbeddedContactPointService) CreateContactPoint(ctx context.Context, orgID int64,
	contactPoint apimodels.EmbeddedContactPoint, provenance models.Provenance) (apimodels.EmbeddedContactPoint, error) {

	if err := contactPoint.IsValid(ecp.encryptionService.GetDecryptedValue); err != nil {
		return apimodels.EmbeddedContactPoint{}, fmt.Errorf("contact point is not valid: %w", err)
	}

	cfg, fetchedHash, err := ecp.getCurrentConfig(ctx, orgID)
	if err != nil {
		return apimodels.EmbeddedContactPoint{}, err
	}

	extracedSecrets, err := contactPoint.ExtractSecrtes()
	if err != nil {
		return apimodels.EmbeddedContactPoint{}, err
	}

	for k, v := range extracedSecrets {
		encryptedValue, err := ecp.encryptValue(v)
		if err != nil {
			return apimodels.EmbeddedContactPoint{}, err
		}
		extracedSecrets[k] = encryptedValue
	}

	contactPoint.UID = util.GenerateShortUID()
	grafanaReceiver := &apimodels.PostableGrafanaReceiver{
		UID:                   contactPoint.UID,
		Name:                  contactPoint.Name,
		Type:                  contactPoint.Type,
		DisableResolveMessage: contactPoint.DisableResolveMessage,
		Settings:              contactPoint.Settings,
		SecureSettings:        extracedSecrets,
	}

	receiverFound := false
	for _, receiver := range cfg.AlertmanagerConfig.Receivers {
		if receiver.Name == contactPoint.Name {
			receiver.PostableGrafanaReceivers.GrafanaManagedReceivers = append(receiver.PostableGrafanaReceivers.GrafanaManagedReceivers, grafanaReceiver)
			receiverFound = true
		}
	}

	if !receiverFound {
		cfg.AlertmanagerConfig.Receivers = append(cfg.AlertmanagerConfig.Receivers, &apimodels.PostableApiReceiver{
			Receiver: config.Receiver{
				Name: grafanaReceiver.Name,
			},
			PostableGrafanaReceivers: apimodels.PostableGrafanaReceivers{
				GrafanaManagedReceivers: []*apimodels.PostableGrafanaReceiver{grafanaReceiver},
			},
		})
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return apimodels.EmbeddedContactPoint{}, err
	}

	err = ecp.xact.InTransaction(ctx, func(ctx context.Context) error {
		err = ecp.amStore.UpdateAlertmanagerConfiguration(ctx, &models.SaveAlertmanagerConfigurationCmd{
			AlertmanagerConfiguration: string(data),
			FetchedConfigurationHash:  fetchedHash,
			ConfigurationVersion:      "v1",
			Default:                   false,
			OrgID:                     orgID,
		})
		if err != nil {
			return err
		}
		adapter := provenanceOrgAdapter{
			inner: &contactPoint,
			orgID: orgID,
		}
		err = ecp.provenanceStore.SetProvenance(ctx, adapter, provenance)
		if err != nil {
			return err
		}
		contactPoint.Provenance = string(provenance)
		return nil
	})
	if err != nil {
		return apimodels.EmbeddedContactPoint{}, err
	}
	return contactPoint, nil
}

func (ecp *EmbeddedContactPointService) UpdateContactPoint(ctx context.Context, orgID int64, contactPoint apimodels.EmbeddedContactPoint) error {
	// set all redacted values with the latest known value from the store
	rawContactPoint, err := ecp.getContactPointUncrypted(ctx, orgID, contactPoint.UID)
	if err != nil {
		return err
	}
	secretKeys, err := contactPoint.SecretKeys()
	if err != nil {
		return err
	}
	for _, secretKey := range secretKeys {
		secretValue := contactPoint.Settings.Get(secretKey).MustString()
		if secretValue == apimodels.RedactedValue {
			contactPoint.Settings.Set(secretKey, rawContactPoint.Settings.Get(secretKey).MustString())
		}
	}
	// validate merged values
	if err := contactPoint.IsValid(ecp.encryptionService.GetDecryptedValue); err != nil {
		return err
	}
	// transform to internal model
	extracedSecrets, err := contactPoint.ExtractSecrtes()
	if err != nil {
		return err
	}
	for k, v := range extracedSecrets {
		encryptedValue, err := ecp.encryptValue(v)
		if err != nil {
			return err
		}
		extracedSecrets[k] = encryptedValue
	}
	mergedReceiver := &apimodels.PostableGrafanaReceiver{
		UID:                   contactPoint.UID,
		Name:                  contactPoint.Name,
		Type:                  contactPoint.Type,
		DisableResolveMessage: contactPoint.DisableResolveMessage,
		Settings:              contactPoint.Settings,
		SecureSettings:        extracedSecrets,
	}
	// save to store
	cfg, fetchedHash, err := ecp.getCurrentConfig(ctx, orgID)
	if err != nil {
		return err
	}
	for _, receiver := range cfg.AlertmanagerConfig.Receivers {
		if receiver.Name == contactPoint.Name {
			receiverNotFound := true
			for i, grafanaReceiver := range receiver.GrafanaManagedReceivers {
				if grafanaReceiver.UID == mergedReceiver.UID {
					receiverNotFound = false
					receiver.GrafanaManagedReceivers[i] = mergedReceiver
					break
				}
			}
			if receiverNotFound {
				return fmt.Errorf("contact point with uid '%s' not found", mergedReceiver.UID)
			}
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return ecp.amStore.UpdateAlertmanagerConfiguration(ctx, &models.SaveAlertmanagerConfigurationCmd{
		AlertmanagerConfiguration: string(data),
		FetchedConfigurationHash:  fetchedHash,
		ConfigurationVersion:      "v1",
		Default:                   false,
		OrgID:                     orgID,
	})
}

func (ecp *EmbeddedContactPointService) DeleteContactPoint(ctx context.Context, orgID int64, uid string) error {
	cfg, fetchedHash, err := ecp.getCurrentConfig(ctx, orgID)
	if err != nil {
		return err
	}
	for _, receiver := range cfg.AlertmanagerConfig.Receivers {
		for index, grafanaReceiver := range receiver.GrafanaManagedReceivers {
			if grafanaReceiver.UID == uid {
				receiver.GrafanaManagedReceivers = append(receiver.GrafanaManagedReceivers[:index], receiver.GrafanaManagedReceivers[index+1:]...)
				break
			}
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return ecp.xact.InTransaction(ctx, func(ctx context.Context) error {
		err := ecp.provenanceStore.DeleteProvenance(ctx, orgID, &apimodels.EmbeddedContactPoint{
			UID: uid,
		})
		if err != nil {
			return err
		}
		return ecp.amStore.UpdateAlertmanagerConfiguration(ctx, &models.SaveAlertmanagerConfigurationCmd{
			AlertmanagerConfiguration: string(data),
			FetchedConfigurationHash:  fetchedHash,
			ConfigurationVersion:      "v1",
			Default:                   false,
			OrgID:                     orgID,
		})
	})
}

func (ecp *EmbeddedContactPointService) getCurrentConfig(ctx context.Context, orgID int64) (*apimodels.PostableUserConfig, string, error) {
	query := &models.GetLatestAlertmanagerConfigurationQuery{
		OrgID: orgID,
	}
	err := ecp.amStore.GetLatestAlertmanagerConfiguration(ctx, query)
	if err != nil {
		return nil, "", err
	}
	cfg, err := DeserializeAlertmanagerConfig([]byte(query.Result.AlertmanagerConfiguration))
	if err != nil {
		return nil, "", err
	}
	return cfg, query.Result.ConfigurationHash, nil
}

func (ecp *EmbeddedContactPointService) decrypteValue(value string) (string, error) {
	decodeValue, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}

	decryptedValue, err := ecp.encryptionService.Decrypt(context.Background(), decodeValue)
	if err != nil {
		return "", err
	}

	return string(decryptedValue), nil
}

func (ecp *EmbeddedContactPointService) encryptValue(value string) (string, error) {
	encryptedData, err := ecp.encryptionService.Encrypt(context.Background(), []byte(value), secrets.WithoutScope())
	if err != nil {
		return "", fmt.Errorf("failed to encrypt secure settings: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encryptedData), nil
}