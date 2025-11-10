package controller

import (
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"
	"golang.org/x/net/idna"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// List DDNS Profiles
// @Summary List DDNS profiles
// @Schemes
// @Description List DDNS profiles
// @Security BearerAuth
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.DDNSProfile]
// @Router /ddns [get]
func listDDNS(c *gin.Context) ([]*model.DDNSProfile, error) {
	var ddnsProfiles []*model.DDNSProfile

	list := singleton.DDNSShared.GetSortedList()
	if err := copier.Copy(&ddnsProfiles, &list); err != nil {
		return nil, err
	}

	return ddnsProfiles, nil
}

// Add DDNS profile
// @Summary Add DDNS profile
// @Security BearerAuth
// @Schemes
// @Description Add DDNS profile
// @Tags auth required
// @Accept json
// @param request body model.DDNSForm true "DDNS Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /ddns [post]
func createDDNS(c *gin.Context) (uint64, error) {
	var df model.DDNSForm
	var p model.DDNSProfile

	if err := c.ShouldBindJSON(&df); err != nil {
		return 0, err
	}

	if df.MaxRetries < 1 || df.MaxRetries > 10 {
		return 0, singleton.Localizer.ErrorT("the retry count must be an integer between 1 and 10")
	}

	p.UserID = getUid(c)
	p.Name = df.Name
	enableIPv4 := df.EnableIPv4
	enableIPv6 := df.EnableIPv6
	p.EnableIPv4 = &enableIPv4
	p.EnableIPv6 = &enableIPv6
	p.MaxRetries = df.MaxRetries
	p.Provider = df.Provider
	p.Domains = df.Domains
	p.AccessID = df.AccessID
	p.AccessSecret = df.AccessSecret
	p.WebhookURL = df.WebhookURL
	p.WebhookMethod = df.WebhookMethod
	p.WebhookRequestType = df.WebhookRequestType
	p.WebhookRequestBody = df.WebhookRequestBody
	p.WebhookHeaders = df.WebhookHeaders

	for n, domain := range p.Domains {
		// IDN to ASCII
		domainValid, domainErr := idna.Lookup.ToASCII(domain)
		if domainErr != nil {
			return 0, singleton.Localizer.ErrorT("error parsing %s: %v", domain, domainErr)
		}
		p.Domains[n] = domainValid
	}

	if err := singleton.DB.Create(&p).Error; err != nil {
		return 0, newGormError("%v", err)
	}

	singleton.DDNSShared.Update(&p)
	return p.ID, nil
}

// Edit DDNS profile
// @Summary Edit DDNS profile
// @Security BearerAuth
// @Schemes
// @Description Edit DDNS profile
// @Tags auth required
// @Accept json
// @param id path uint true "Profile ID"
// @param request body model.DDNSForm true "DDNS Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /ddns/{id} [patch]
func updateDDNS(c *gin.Context) (any, error) {
	idStr := c.Param("id")

	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	var df model.DDNSForm
	if err := c.ShouldBindJSON(&df); err != nil {
		return nil, err
	}

	if df.MaxRetries < 1 || df.MaxRetries > 10 {
		return nil, singleton.Localizer.ErrorT("the retry count must be an integer between 1 and 10")
	}

	var p model.DDNSProfile
	if err = singleton.DB.First(&p, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("profile id %d does not exist", id)
	}

	if !p.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	p.Name = df.Name
	enableIPv4 := df.EnableIPv4
	enableIPv6 := df.EnableIPv6
	p.EnableIPv4 = &enableIPv4
	p.EnableIPv6 = &enableIPv6
	p.MaxRetries = df.MaxRetries
	p.Provider = df.Provider
	p.Domains = df.Domains
	p.AccessID = df.AccessID
	p.AccessSecret = df.AccessSecret
	p.WebhookURL = df.WebhookURL
	p.WebhookMethod = df.WebhookMethod
	p.WebhookRequestType = df.WebhookRequestType
	p.WebhookRequestBody = df.WebhookRequestBody
	p.WebhookHeaders = df.WebhookHeaders

	for n, domain := range p.Domains {
		// IDN to ASCII
		domainValid, domainErr := idna.Lookup.ToASCII(domain)
		if domainErr != nil {
			return nil, singleton.Localizer.ErrorT("error parsing %s: %v", domain, domainErr)
		}
		p.Domains[n] = domainValid
	}

	if err = singleton.DB.Save(&p).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.DDNSShared.Update(&p)

	return nil, nil
}

// Batch delete DDNS configurations
// @Summary Batch delete DDNS configurations
// @Security BearerAuth
// @Schemes
// @Description Batch delete DDNS configurations
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/ddns [post]
func batchDeleteDDNS(c *gin.Context) (any, error) {
	var ddnsConfigs []uint64

	if err := c.ShouldBindJSON(&ddnsConfigs); err != nil {
		return nil, err
	}

	if !singleton.DDNSShared.CheckPermission(c, slices.Values(ddnsConfigs)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	if err := singleton.DB.Unscoped().Delete(&model.DDNSProfile{}, "id in (?)", ddnsConfigs).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.DDNSShared.Delete(ddnsConfigs)
	return nil, nil
}

// List DDNS Providers
// @Summary List DDNS providers
// @Schemes
// @Description List DDNS providers
// @Security BearerAuth
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]string]
// @Router /ddns/providers [get]
func listProviders(c *gin.Context) ([]string, error) {
	return model.ProviderList[:], nil
}
