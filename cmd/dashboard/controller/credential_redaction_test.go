package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func TestGetProfile_RedactsPasswordHash(t *testing.T) {
	defer setupTenancyTest(t)()
	require.NoError(t, singleton.DB.AutoMigrate(&model.Oauth2Bind{}))

	passwordHash := "$2a$10$profilePasswordHashMustNotLeak012345678901"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/profile", nil)
	c.Set(model.CtxKeyAuthorizedUser, &model.User{
		Common:   model.Common{ID: 10},
		Username: "alice",
		Password: passwordHash,
		Role:     model.RoleMember,
	})

	commonHandler(getProfile)(c)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), passwordHash)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, true, body["success"])
	data, ok := body["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "alice", data["username"])
	require.NotContains(t, data, "password")
}

func TestUpdateProfile_UsesStoredPasswordHashAfterRedaction(t *testing.T) {
	defer setupTenancyTest(t)()
	require.NoError(t, singleton.DB.AutoMigrate(&model.Oauth2Bind{}, &model.JWTSession{}, &model.APIToken{}))

	originalUserInfoMap := singleton.UserInfoMap
	originalAgentSecretToUserID := singleton.AgentSecretToUserId
	singleton.UserInfoMap = make(map[uint64]model.UserInfo)
	singleton.AgentSecretToUserId = make(map[string]uint64)
	defer func() {
		singleton.UserInfoMap = originalUserInfoMap
		singleton.AgentSecretToUserId = originalAgentSecretToUserID
	}()

	oldHash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	require.NoError(t, err)
	user := model.User{
		Common:      model.Common{ID: 10},
		Username:    "alice",
		Password:    string(oldHash),
		Role:        model.RoleMember,
		AgentSecret: "profile-update-agent-secret",
	}
	require.NoError(t, singleton.DB.Create(&user).Error)

	_, err = updateProfile(profileUpdateContext(t, user, model.ProfileForm{
		OriginalPassword: "wrong-password",
		NewUsername:      "mallory",
		NewPassword:      "new-password",
	}))
	require.Error(t, err)

	var unchanged model.User
	require.NoError(t, singleton.DB.First(&unchanged, user.ID).Error)
	require.Equal(t, "alice", unchanged.Username)
	require.Equal(t, string(oldHash), unchanged.Password)

	_, err = updateProfile(profileUpdateContext(t, user, model.ProfileForm{
		OriginalPassword: "old-password",
		NewUsername:      "alice-renamed",
		NewPassword:      "new-password",
	}))
	require.NoError(t, err)

	var after model.User
	require.NoError(t, singleton.DB.First(&after, user.ID).Error)
	require.Equal(t, "alice-renamed", after.Username)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(after.Password), []byte("new-password")))
}

func profileUpdateContext(t *testing.T, user model.User, form model.ProfileForm) *gin.Context {
	t.Helper()

	body, err := json.Marshal(form)
	require.NoError(t, err)
	c := ctxAs(user.ID, user.Role)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/profile", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(model.CtxKeyAuthorizedUser, &user)
	return c
}

func TestListDDNS_RedactsCredentials(t *testing.T) {
	defer setupTenancyTest(t)()

	p := model.DDNSProfile{
		Common:             model.Common{UserID: 10},
		Name:               "cf",
		Provider:           "cloudflare",
		AccessID:           "id",
		AccessSecret:       "super-secret-token",
		WebhookURL:         "https://ddns.example/update?token=embedded-secret",
		WebhookRequestBody: `{"token":"embedded-secret"}`,
		WebhookHeaders:     `{"Authorization":"Bearer xxx"}`,
	}
	require.NoError(t, singleton.DB.Create(&p).Error)
	singleton.DDNSShared.InsertForTest(&p)

	c := ctxAs(10, model.RoleAdmin)
	out, err := listDDNS(c)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Empty(t, out[0].AccessSecret, "access_secret must be redacted in list response")
	require.Empty(t, out[0].WebhookURL, "webhook_url must be redacted in list response")
	require.Empty(t, out[0].WebhookRequestBody, "webhook_request_body must be redacted in list response")
	require.Empty(t, out[0].WebhookHeaders, "webhook_headers must be redacted in list response")
	require.Equal(t, "id", out[0].AccessID, "non-secret fields must be preserved")

	var stored model.DDNSProfile
	require.NoError(t, singleton.DB.First(&stored, p.ID).Error)
	require.Equal(t, "super-secret-token", stored.AccessSecret, "redaction must not mutate stored data")
	require.Equal(t, "https://ddns.example/update?token=embedded-secret", stored.WebhookURL,
		"redaction must not mutate stored webhook URL")
	require.Equal(t, `{"token":"embedded-secret"}`, stored.WebhookRequestBody,
		"redaction must not mutate stored webhook body")
}

func TestListNotification_RedactsCredentials(t *testing.T) {
	defer setupTenancyTest(t)()

	n := model.Notification{
		Common:        model.Common{UserID: 10},
		Name:          "slack",
		URL:           "https://hooks.slack.com/services/T/B/secret",
		RequestHeader: `{"Authorization":"Bearer xxx"}`,
		RequestBody:   `{"text":"#NEZHA#"}`,
	}
	require.NoError(t, singleton.DB.Create(&n).Error)
	singleton.NotificationShared.InsertForTest(&n)

	c := ctxAs(10, model.RoleAdmin)
	out, err := listNotification(c)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Empty(t, out[0].URL, "url must be redacted in list response")
	require.Empty(t, out[0].RequestHeader, "request_header must be redacted in list response")
	require.Empty(t, out[0].RequestBody, "request_body must be redacted in list response")
	require.Equal(t, "slack", out[0].Name, "non-secret fields must be preserved")

	var stored model.Notification
	require.NoError(t, singleton.DB.First(&stored, n.ID).Error)
	require.Equal(t, "https://hooks.slack.com/services/T/B/secret", stored.URL,
		"redaction must not mutate stored data")
}

func TestUpdateDDNS_EmptySecretPreservesStored(t *testing.T) {
	defer setupTenancyTest(t)()

	existing := model.DDNSProfile{
		Common:             model.Common{UserID: 10},
		Name:               "cf",
		Provider:           "webhook",
		AccessID:           "id",
		AccessSecret:       "keep-me",
		WebhookURL:         "http://127.0.0.1/",
		WebhookRequestBody: `{"X-Token":"keep-body"}`,
		WebhookMethod:      1,
		WebhookHeaders:     `{"X-Token":"keep-header"}`,
	}
	require.NoError(t, singleton.DB.Create(&existing).Error)
	singleton.DDNSShared.InsertForTest(&existing)

	c := ctxAsMemberWithBody(10, map[string]any{
		"name":                 "cf-renamed",
		"provider":             "webhook",
		"access_id":            "id",
		"access_secret":        "",
		"webhook_url":          "",
		"webhook_method":       1,
		"webhook_request_type": 1,
		"webhook_headers":      "",
		"webhook_request_body": "",
		"max_retries":          3,
	})
	c.Params = gin.Params{{Key: "id", Value: itoa(existing.ID)}}
	_, err := updateDDNS(c)
	require.NoError(t, err)

	var after model.DDNSProfile
	require.NoError(t, singleton.DB.First(&after, existing.ID).Error)
	require.Equal(t, "cf-renamed", after.Name, "non-secret edits must apply")
	require.Equal(t, "keep-me", after.AccessSecret, "empty submitted secret must preserve stored value")
	require.Equal(t, `{"X-Token":"keep-header"}`, after.WebhookHeaders,
		"empty submitted headers must preserve stored value")
	require.Equal(t, "http://127.0.0.1/", after.WebhookURL,
		"empty submitted webhook URL must preserve stored value")
	require.Equal(t, `{"X-Token":"keep-body"}`, after.WebhookRequestBody,
		"empty submitted webhook body must preserve stored value")
}

func TestUpdateDDNS_NonEmptySecretOverwrites(t *testing.T) {
	defer setupTenancyTest(t)()

	existing := model.DDNSProfile{
		Common:        model.Common{UserID: 10},
		Name:          "cf",
		Provider:      "webhook",
		AccessSecret:  "old",
		WebhookURL:    "http://127.0.0.1/",
		WebhookMethod: 1,
	}
	require.NoError(t, singleton.DB.Create(&existing).Error)
	singleton.DDNSShared.InsertForTest(&existing)

	c := ctxAsMemberWithBody(10, map[string]any{
		"name":                 "cf",
		"provider":             "webhook",
		"access_secret":        "new-secret",
		"webhook_url":          "http://127.0.0.1/",
		"webhook_method":       1,
		"webhook_request_type": 1,
		"max_retries":          3,
	})
	c.Params = gin.Params{{Key: "id", Value: itoa(existing.ID)}}
	_, err := updateDDNS(c)
	require.NoError(t, err)

	var after model.DDNSProfile
	require.NoError(t, singleton.DB.First(&after, existing.ID).Error)
	require.Equal(t, "new-secret", after.AccessSecret, "non-empty submitted secret must overwrite")
}

func TestUpdateNotification_EmptyFieldsPreserveStored(t *testing.T) {
	defer setupTenancyTest(t)()

	existing := model.Notification{
		Common:        model.Common{UserID: 10},
		Name:          "slack",
		URL:           "https://hooks.slack.com/services/keep",
		RequestMethod: model.NotificationRequestMethodGET,
		RequestType:   model.NotificationRequestTypeJSON,
		RequestHeader: `{"Authorization":"keep"}`,
		RequestBody:   "",
	}
	require.NoError(t, singleton.DB.Create(&existing).Error)
	singleton.NotificationShared.InsertForTest(&existing)

	c := ctxAsMemberWithBody(10, map[string]any{
		"name":           "slack-renamed",
		"url":            "",
		"request_method": model.NotificationRequestMethodGET,
		"request_type":   model.NotificationRequestTypeJSON,
		"request_header": "",
		"request_body":   "",
		"skip_check":     true,
	})
	c.Params = gin.Params{{Key: "id", Value: itoa(existing.ID)}}
	_, err := updateNotification(c)
	require.NoError(t, err)

	var after model.Notification
	require.NoError(t, singleton.DB.First(&after, existing.ID).Error)
	require.Equal(t, "slack-renamed", after.Name, "non-secret edits must apply")
	require.Equal(t, "https://hooks.slack.com/services/keep", after.URL,
		"empty submitted url must preserve stored value")
	require.Equal(t, `{"Authorization":"keep"}`, after.RequestHeader,
		"empty submitted request_header must preserve stored value")
}
