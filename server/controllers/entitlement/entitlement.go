package entitlement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"server/config"
	"server/model"
	"server/persistence"
	"server/telemetry"
	"server/util"
	"sync"

	log "github.com/sirupsen/logrus"
)

type EntitlementAPIController struct {
	ctx            context.Context
	httpRequester  *util.HttpRequester
	apiUrl         string
	productCode    string
	subscriptionId string
	tenantId       string
	database       *persistence.Database
}

type APIFilter struct {
	VendorProductCode   string `json:"vendorProductCode,omitempty"`
	AzureSubscriptionId string `json:"azureSubscriptionId,omitempty"`
	PartnerCode         string `json:"partnerCode"`
}

type APIEntitlementRequest struct {
	AccountId           string `json:"accountId"`
	AzureSubscriptionId string `json:"azureSubscriptionId"`
	AzureTenantId       string `json:"azureTenantId"`
	PartnerProductCode  string `json:"partnerProductCode"`
}

type APIEntitlementResponse struct {
	AccountId string `json:"accountId"`
}

type APIResponseContent struct {
	SourcePartner     string
	RhAccountId       string
	Status            string
	PartnerIdentities map[string]string
	RhEntitlements    []map[string]string
}

type APIResponse struct {
	Content []APIResponseContent
	Page    map[string]float64
}

var (
	once                    sync.Once
	entitlementCtrlInstance *EntitlementAPIController
)

const PARTNER_SUBSCRIPTION_ENDPOINT string = "partnerSubscriptions"
const PARTNER_ENTITLEMENT_ENDPOINT string = "partnerEntitlements"
const PARTNER_TYPE_CODE string = "azure"

func NewEntitlementController(context context.Context, db *persistence.Database) *EntitlementAPIController {
	once.Do(func() {

		cert := config.GetEnvironment().SW_SUB_API_CERTIFICATE
		key := config.GetEnvironment().SW_SUB_API_PRIVATEKEY
		url := config.GetEnvironment().SW_SUB_API_URL
		code := config.GetEnvironment().SW_SUB_VENDOR_PRODUCT_CODE
		subs := config.GetEnvironment().SUBSCRIPTION
		tenant := config.GetEnvironment().AZURE_TENANT_ID
		var requester *util.HttpRequester

		if cert == "" || key == "" {
			log.Warn("Entitlements controller will not be initialized because certificate or key are not provided.")
		} else {
			var err error
			requester, err = util.NewHttpRequesterWithCertificate(cert, key)
			if err != nil {
				log.Warnf("Could not initialize entitlements controller. %v\n", err)
			}
		}
		entitlementCtrlInstance = &EntitlementAPIController{
			ctx:            context,
			httpRequester:  requester,
			apiUrl:         url,
			productCode:    code,
			subscriptionId: subs,
			tenantId:       tenant,
			database:       db,
		}
	})
	return entitlementCtrlInstance
}

func (controller *EntitlementAPIController) RequestEntitlementCreation(orgId string) {
	if controller.httpRequester != nil {
		req := APIEntitlementRequest{
			AccountId:           orgId,
			AzureSubscriptionId: controller.subscriptionId,
			AzureTenantId:       controller.tenantId,
			PartnerProductCode:  controller.productCode,
		}
		endpoint, err := url.JoinPath(controller.apiUrl, PARTNER_ENTITLEMENT_ENDPOINT, PARTNER_TYPE_CODE)
		if err != nil {
			log.Warnf("Failed to create entitlement URL: %v", err)
			storeError(controller.database, err)
			return
		}
		// TODO Change this to trace in the future, adding as info to collect data
		log.Infof("Calling create entitlement API at URL %s with content %+v", endpoint, req)

		resp, err := controller.httpRequester.MakeRequestWithJSONBody(
			controller.ctx,
			"POST",
			endpoint,
			nil,
			req,
		)
		if err != nil {
			log.Warnf("Failed to get response from entitlement API: %v", err)
			storeError(controller.database, err)
			return
		}
		if resp.StatusCode != http.StatusOK {
			errStr := fmt.Sprintf("Entitlements API returned error status code %d - content %v", resp.StatusCode, string(resp.Body[:]))
			log.Error(errStr)
			storeError(controller.database, errors.New(errStr))
			return
		}
		response := APIEntitlementResponse{}
		if err := json.Unmarshal(resp.Body, &response); err != nil {
			log.Warnf("Couldn't unmarshal JSON response. %v", err)
			storeError(controller.database, err)
			return
		}
		// TODO Change this to trace in the future, adding as info to collect data
		log.Infof("Create entitlement API returned: %+v", response)
		if response.AccountId == "" {
			log.Warn("AAP entitlement creation API returned no entitled account ID.")
		} else if response.AccountId == orgId {
			log.Infof("AAP entitlement created or already existed for account: %s", response.AccountId)
		} else {
			log.Infof("AAP entitlement for this tenant/subscription already bound to org ID: %s", response.AccountId)
		}
		err = telemetry.SendEntitlementResult(orgId, response.AccountId)
		if err != nil {
			log.Errorf("Unable to send entitlement result to segment: %v", err)
		}
		return
	}
	log.Warn("Entitlements can not be created, entitlement controller was not initialized.")
}

func (controller *EntitlementAPIController) FetchSubscriptions() {

	// no need to wait for this one, its not long running and http request uses context
	go func() {
		if controller.httpRequester != nil {
			// in the future we might need to handle pagination... maybe...
			endpoint, err := url.JoinPath(controller.apiUrl, PARTNER_SUBSCRIPTION_ENDPOINT)
			if err != nil {
				log.Warnf("Failed to create entitlement URL: %v", err)
				storeError(controller.database, err)
				return
			}
			resp, err := controller.httpRequester.MakeRequestWithJSONBody(
				controller.ctx,
				"POST",
				endpoint,
				nil,
				APIFilter{
					VendorProductCode:   controller.productCode,
					AzureSubscriptionId: controller.subscriptionId,
					PartnerCode:         PARTNER_TYPE_CODE,
				},
			)
			if err != nil {
				log.Warnf("Failed to get response from subscription API: %v", err)
				storeError(controller.database, err)
				return
			}

			if resp.StatusCode != http.StatusOK {
				errStr := fmt.Sprintf("Subscription API returned error status code %d - content %v", resp.StatusCode, string(resp.Body[:]))
				log.Error(errStr)
				storeError(controller.database, errors.New(errStr))
				return
			}

			response := APIResponse{}
			if err := json.Unmarshal(resp.Body, &response); err != nil {
				log.Warnf("Couldn't unmarshal JSON response. %v", err)
				storeError(controller.database, err)
				return
			}
			log.Tracef("Entitlements check response: %+v", response)
			storeEntitlements(controller.database, &response)

			return
		}
		log.Warn("Entitlements can not be fetched, entitlement controller was not initialized.")
	}()
}

func storeError(db *persistence.Database, err error) {
	if err != nil {
		entitlement := model.AzureMarketplaceEntitlement{
			ErrorMessage: err.Error(),
		}
		persistRecord(db, &entitlement)
	}
}

func storeEntitlements(db *persistence.Database, data *APIResponse) {
	if len(data.Content) == 0 {
		log.Info("No entitlements found.  Empty response.")
		return
	}

	for _, c := range data.Content {
		// supporting only Azure marketplace entitlements for now
		if c.SourcePartner == "azure_marketplace" {

			var azSubId, azCustId string
			var exists bool
			if azSubId, exists = c.PartnerIdentities["azureSubscriptionId"]; !exists {
				azSubId = ""
			}
			if azCustId, exists = c.PartnerIdentities["azureCustomerId"]; !exists {
				azCustId = ""
			}
			entitlement := model.AzureMarketplaceEntitlement{
				AzureSubscriptionId: azSubId,
				AzureCustomerId:     azCustId,
				RHEntitlements:      make([]model.RedHatEntitlements, 0),
				RedHatAccountId:     c.RhAccountId,
				Status:              c.Status,
			}
			if len(c.RhEntitlements) == 0 {
				log.Info("No Red Hat entitlements found for this Azure tenant/subscription.")
			} else {
				for _, rhe := range c.RhEntitlements {
					var sku, subNum string
					var skuExists, subNumExists bool
					sku, skuExists = rhe["sku"]
					subNum, subNumExists = rhe["subscriptionNumber"]
					if skuExists && subNumExists {
						log.Tracef("Red Hat entitlement found: %s", subNum)
						entitlement.RHEntitlements = append(entitlement.RHEntitlements, model.RedHatEntitlements{
							Sku:                sku,
							SubscriptionNumber: subNum,
						})
					}
				}
				persistRecord(db, &entitlement)
			}
		} else {
			log.Info("No azure_marketplace entitlements found.")
		}
	}
}

func persistRecord(db *persistence.Database, entitlement *model.AzureMarketplaceEntitlement) {
	tx := db.Instance.Save(entitlement)
	if tx.Error != nil {
		log.Warnf("Failed to persist Azure Marketplace Entitlement record: %v", tx.Error.Error())
	}
}
