package singleton

import (
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/frontend"
	"github.com/nezhahq/nezha/pkg/utils"
)

var Conf *ConfigClass

type ConfigClass struct {
	*model.Config

	onlineHistoryRetentionMu sync.RWMutex
	frontendSelectionMu      sync.RWMutex
	natDomainPolicyMu        sync.RWMutex
	FrontendStore            *frontend.Store `json:"-"`

	IgnoredIPNotificationServerIDs map[uint64]bool `json:"ignored_ip_notification_server_ids,omitempty"`
	Oauth2Providers                []string        `json:"oauth2_providers,omitempty"`
}

// InitConfigFromPath 从给出的文件路径中加载配置
func InitConfigFromPath(path string) error {
	store, err := frontend.NewStore(filepath.Join(filepath.Dir(path), "frontends"), FrontendTemplates)
	if err != nil {
		return err
	}
	Conf = &ConfigClass{
		Config:        &model.Config{},
		FrontendStore: store,
	}
	err = Conf.Read(path, store.Templates())
	if err != nil {
		return err
	}
	rotated, err := Conf.RotateJWTSecretKeyIfNeeded(Version)
	if err != nil {
		return err
	}
	if rotated {
		log.Printf("NEZHA>> Rotated jwt_secret_key for dashboard version %s", Version)
	}

	Conf.updateIgnoredIPNotificationID()
	Conf.Oauth2Providers = utils.MapKeysToSlice(Conf.Oauth2)
	return nil
}

func (c *ConfigClass) Save() error {
	c.updateIgnoredIPNotificationID()
	return c.Config.Save()
}

// updateIgnoredIPNotificationID 更新用于判断服务器ID是否属于特定服务器的map
func (c *ConfigClass) updateIgnoredIPNotificationID() {
	if c.IgnoredIPNotification == "" {
		return
	}

	c.IgnoredIPNotificationServerIDs = make(map[uint64]bool)
	for splitedID := range strings.SplitSeq(c.IgnoredIPNotification, ",") {
		id, _ := strconv.ParseUint(splitedID, 10, 64)
		if id > 0 {
			c.IgnoredIPNotificationServerIDs[id] = true
		}
	}
}

// OnlineHistoryRetentionDays returns the retention policy visible to queries and pruning.
func (c *ConfigClass) OnlineHistoryRetentionDays() int {
	c.onlineHistoryRetentionMu.RLock()
	defer c.onlineHistoryRetentionMu.RUnlock()
	return c.OnlineStatsDays
}

// CommitOnlineHistoryRetention applies a retention change with the settings save.
func (c *ConfigClass) CommitOnlineHistoryRetention(days *int, persist func() error) error {
	// Readers must not capture a shorter cutoff while Save can still fail.
	// Serialize assignment, persistence and rollback with the policy snapshot.
	c.onlineHistoryRetentionMu.Lock()
	defer c.onlineHistoryRetentionMu.Unlock()
	previous := c.OnlineStatsDays
	if days != nil {
		c.OnlineStatsDays = *days
	}
	if err := persist(); err != nil {
		c.OnlineStatsDays = previous
		return err
	}
	return nil
}

// SettingsSnapshot copies response fields without copying the retention mutex.
func (c *ConfigClass) SettingsSnapshot() model.Setting {
	c.frontendSelectionMu.RLock()
	defer c.frontendSelectionMu.RUnlock()
	c.natDomainPolicyMu.RLock()
	defer c.natDomainPolicyMu.RUnlock()
	c.onlineHistoryRetentionMu.RLock()
	defer c.onlineHistoryRetentionMu.RUnlock()
	return model.Setting{
		NATMemberDomains:               append([]string(nil), c.NATMemberDomains...),
		ConfigForGuests:                c.ConfigForGuests,
		ConfigDashboard:                c.ConfigDashboard,
		IgnoredIPNotificationServerIDs: c.IgnoredIPNotificationServerIDs,
		Oauth2Providers:                c.Oauth2Providers,
	}
}

func (c *ConfigClass) AvailableFrontendTemplates() []model.FrontendTemplate {
	if c.FrontendStore != nil {
		return c.FrontendStore.Templates()
	}
	return append([]model.FrontendTemplate(nil), FrontendTemplates...)
}

func (c *ConfigClass) FrontendSelection() (user, admin string) {
	c.frontendSelectionMu.RLock()
	defer c.frontendSelectionMu.RUnlock()
	return c.UserTemplate, c.AdminTemplate
}

// CommitFrontendSelection publishes the selection only after persistence succeeds.
// Requests already serving the previous version keep their immutable directory.
func (c *ConfigClass) CommitFrontendSelection(user, admin *string, persist func() error) error {
	c.frontendSelectionMu.Lock()
	defer c.frontendSelectionMu.Unlock()
	previousUser, previousAdmin := c.UserTemplate, c.AdminTemplate
	if user != nil {
		c.UserTemplate = *user
	}
	if admin != nil {
		c.AdminTemplate = *admin
	}
	if err := persist(); err != nil {
		c.UserTemplate, c.AdminTemplate = previousUser, previousAdmin
		return err
	}
	return nil
}
