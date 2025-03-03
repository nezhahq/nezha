package singleton

import (
	"strconv"
	"strings"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

var Conf *ConfigClass

type ConfigClass struct {
	*model.Config

	IgnoredIPNotificationServerIDs map[uint64]bool `json:"ignored_ip_notification_server_ids,omitempty"`
	Oauth2Providers                []string        `json:"oauth2_providers,omitempty"`
}

// InitConfigFromPath 从给出的文件路径中加载配置
func InitConfigFromPath(path string) error {
	Conf = &ConfigClass{
		Config: &model.Config{},
	}
	err := Conf.Read(path, FrontendTemplates)
	if err != nil {
		return err
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
