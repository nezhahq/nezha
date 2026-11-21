package controller

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/backup"
	"github.com/nezhahq/nezha/service/singleton"
)

// Backups contain every credential of the dashboard, so all backup routes
// are restricted to administrators signed in through the web UI: API tokens
// are refused (patForbidden) and adminHandler checks the role. State changing
// routes are covered by the group-wide CSRF middleware.
func registerBackupRoutes(auth *gin.RouterGroup, patForbidden gin.HandlerFunc) {
	b := auth.Group("/backup", patForbidden)
	b.GET("", adminHandler(listBackups))
	b.POST("", adminHandler(createBackup))
	b.GET("/config", adminHandler(getBackupConfig))
	b.PATCH("/config", adminHandler(updateBackupConfig))
	b.POST("/config/test", adminHandler(testBackupStorage))
	b.POST("/password", adminHandler(setBackupPassword))
	b.PATCH("/password", adminHandler(changeBackupPassword))
	b.POST("/password/reset", adminHandler(resetBackupPassword))
	b.GET("/restore/status", adminHandler(getRestoreStatus))
	b.POST("/restore/prepare", adminHandler(prepareRestore))
	b.POST("/restore/apply", adminHandler(applyRestore))
	b.DELETE("/restore/:token", adminHandler(cancelRestore))
	b.GET("/:name/download", adminHandler(downloadBackup))
	auth.POST("/batch-delete/backup", patForbidden, adminHandler(batchDeleteBackup))
}

// backupUploadPath is the only route whose body may exceed the API limit.
const backupUploadPath = "/api/v1/backup/restore/prepare"

// backupUploadBodyLimit is the body limit of the restore upload: the backup
// file plus room for the multipart framing and the text fields.
func backupUploadBodyLimit() int64 {
	if singleton.BackupShared == nil {
		return restAPIRequestMaxBodyBytes
	}
	return singleton.BackupShared.MaxBackupBytes() + 1<<20
}

// reauthWindow is how recent the login of an administrator without a
// password (OAuth only) must be to apply a restore.
const reauthWindow = 10 * time.Minute

var (
	errBackupUnavailable  = errors.New("backups are unavailable on this dashboard")
	errLoginPasswordNeed  = errors.New("enter your login password to apply the restore")
	errLoginPasswordWrong = errors.New("wrong login password")
	errReauthNeeded       = errors.New("sign in again (within the last 10 minutes) to apply the restore")
)

func backupManager() (*backup.Manager, error) {
	if singleton.BackupShared == nil {
		return nil, errBackupUnavailable
	}
	return singleton.BackupShared, nil
}

func backupActor(c *gin.Context) backup.Actor {
	a := backup.Actor{SessionID: c.GetString(jwtClaimKeyID), IP: c.GetString(model.CtxKeyRealIPStr)}
	if u, ok := c.Get(model.CtxKeyAuthorizedUser); ok {
		if user, ok := u.(*model.User); ok {
			a.UserID = user.ID
			a.Username = user.Username
		}
	}
	return a
}

// Get backup configuration
// @Summary Get backup configuration
// @Security BearerAuth
// @Schemes
// @Description Storage, schedule and key information. Secrets are never returned.
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[backup.ConfigView]
// @Router /backup/config [get]
func getBackupConfig(c *gin.Context) (*backup.ConfigView, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	v := m.View()
	return &v, nil
}

// Update backup configuration
// @Summary Update backup configuration
// @Security BearerAuth
// @Schemes
// @Description Omitted fields are kept. An empty secret_access_key keeps the saved secret, except that changing endpoint or bucket requires it again.
// @Tags admin required
// @Accept json
// @param request body backup.ConfigUpdate true "changes"
// @Produce json
// @Success 200 {object} model.CommonResponse[backup.ConfigView]
// @Router /backup/config [patch]
func updateBackupConfig(c *gin.Context) (*backup.ConfigView, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	var u backup.ConfigUpdate
	if err := c.ShouldBindJSON(&u); err != nil {
		return nil, err
	}
	v, err := m.UpdateConfig(u, backupActor(c))
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Test backup storage
// @Summary Test backup storage
// @Security BearerAuth
// @Schemes
// @Description Writes, reads and deletes a probe object with the given (possibly unsaved) storage settings. The rules of the update apply.
// @Tags admin required
// @Accept json
// @param request body backup.S3Update true "storage settings"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /backup/config/test [post]
func testBackupStorage(c *gin.Context) (any, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	var u backup.S3Update
	if err := c.ShouldBindJSON(&u); err != nil {
		return nil, err
	}
	return nil, m.TestConnection(c.Request.Context(), &u, backupActor(c))
}

type backupPasswordForm struct {
	Password string `json:"password"`
}

type backupPasswordChangeForm struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

type backupPasswordResetForm struct {
	NewPassword string `json:"new_password"`
	Confirm     string `json:"confirm"`
}

// BackupKeyResponse is returned when a key pair is created. The recovery key
// is shown only this once.
type BackupKeyResponse struct {
	RecoveryKey string    `json:"recovery_key,omitempty"`
	KeyID       string    `json:"key_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// Set backup password
// @Summary Set backup password
// @Security BearerAuth
// @Schemes
// @Description Creates the backup key pair protected by the password and returns the recovery key once.
// @Tags admin required
// @Accept json
// @param request body backupPasswordForm true "password"
// @Produce json
// @Success 200 {object} model.CommonResponse[BackupKeyResponse]
// @Router /backup/password [post]
func setBackupPassword(c *gin.Context) (*BackupKeyResponse, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	var f backupPasswordForm
	if err := c.ShouldBindJSON(&f); err != nil {
		return nil, err
	}
	recovery, k, err := m.SetPassword(f.Password, backupActor(c))
	if err != nil {
		return nil, err
	}
	noStore(c)
	return &BackupKeyResponse{RecoveryKey: recovery, KeyID: k.KeyID, CreatedAt: k.CreatedAt}, nil
}

// Change backup password
// @Summary Change backup password
// @Security BearerAuth
// @Schemes
// @Description Re-wraps the private key; the key pair and the recovery key stay the same.
// @Tags admin required
// @Accept json
// @param request body backupPasswordChangeForm true "passwords"
// @Produce json
// @Success 200 {object} model.CommonResponse[BackupKeyResponse]
// @Router /backup/password [patch]
func changeBackupPassword(c *gin.Context) (*BackupKeyResponse, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	var f backupPasswordChangeForm
	if err := c.ShouldBindJSON(&f); err != nil {
		return nil, err
	}
	k, err := m.ChangePassword(f.OldPassword, f.NewPassword, backupActor(c))
	if err != nil {
		return nil, err
	}
	return &BackupKeyResponse{KeyID: k.KeyID, CreatedAt: k.CreatedAt}, nil
}

// Reset backup password
// @Summary Reset backup password
// @Security BearerAuth
// @Schemes
// @Description Creates a new key pair. Earlier backups then need the old password or the old recovery key. confirm must be "RESET".
// @Tags admin required
// @Accept json
// @param request body backupPasswordResetForm true "new password"
// @Produce json
// @Success 200 {object} model.CommonResponse[BackupKeyResponse]
// @Router /backup/password/reset [post]
func resetBackupPassword(c *gin.Context) (*BackupKeyResponse, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	var f backupPasswordResetForm
	if err := c.ShouldBindJSON(&f); err != nil {
		return nil, err
	}
	recovery, k, err := m.ResetPassword(f.NewPassword, f.Confirm, backupActor(c))
	if err != nil {
		return nil, err
	}
	noStore(c)
	return &BackupKeyResponse{RecoveryKey: recovery, KeyID: k.KeyID, CreatedAt: k.CreatedAt}, nil
}

// List backups
// @Summary List backups
// @Security BearerAuth
// @Schemes
// @Description Stored backups merged with running and recently failed ones, newest first.
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]backup.BackupInfo]
// @Router /backup [get]
func listBackups(c *gin.Context) ([]backup.BackupInfo, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	return m.List(c.Request.Context())
}

// Create backup
// @Summary Create backup
// @Security BearerAuth
// @Schemes
// @Description Starts a backup in the background and returns its running entry.
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[backup.BackupInfo]
// @Router /backup [post]
func createBackup(c *gin.Context) (*backup.BackupInfo, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	b, err := m.StartBackup(backup.TriggerManual, backupActor(c))
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// Batch delete backups
// @Summary Batch delete backups
// @Security BearerAuth
// @Schemes
// @Description Deletes the named backups from storage.
// @Tags admin required
// @Accept json
// @param request body []string true "backup names"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/backup [post]
func batchDeleteBackup(c *gin.Context) (any, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	var names []string
	if err := c.ShouldBindJSON(&names); err != nil {
		return nil, err
	}
	return nil, m.Delete(c.Request.Context(), names, backupActor(c))
}

// Download backup
// @Summary Download backup
// @Security BearerAuth
// @Schemes
// @Description Streams the encrypted .nzbak file through the dashboard.
// @Tags admin required
// @Param name path string true "backup name"
// @Produce octet-stream
// @Success 200 {file} file
// @Router /backup/{name}/download [get]
func downloadBackup(c *gin.Context) (any, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	name := c.Param("name")
	rc, info, err := m.Open(c.Request.Context(), name, backupActor(c))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	h := c.Writer.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + backup.FileExt}))
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if sum := info.Metadata["sha256"]; sum != "" {
		h.Set("X-Backup-SHA256", sum)
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
	return nil, errNoop
}

// restoreApplyRequires tells the frontend which re-authentication Apply will ask for.
type restoreApplyRequires struct {
	LoginPassword bool `json:"login_password"`
	ReauthNeeded  bool `json:"reauth_needed"`
}

// RestorePrepareResponse is the result of a restore check.
type RestorePrepareResponse struct {
	*backup.PrepareResult
	ApplyRequires restoreApplyRequires `json:"apply_requires"`
}

type restorePrepareForm struct {
	Name        string `json:"name"`
	SHA256      string `json:"sha256"`
	Password    string `json:"password"`
	RecoveryKey string `json:"recovery_key"`
}

// Check and stage a restore
// @Summary Check and stage a restore
// @Security BearerAuth
// @Schemes
// @Description Downloads the selected backup (name + sha256 from the list) or takes an uploaded file (multipart field "file", plus "password" or "recovery_key"), decrypts and verifies it and stages it. Returns a single-use restore token bound to this session when every check passed.
// @Tags admin required
// @Accept json
// @Accept multipart/form-data
// @param request body restorePrepareForm false "selected backup and credential"
// @Produce json
// @Success 200 {object} model.CommonResponse[RestorePrepareResponse]
// @Router /backup/restore/prepare [post]
func prepareRestore(c *gin.Context) (*RestorePrepareResponse, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	req := backup.PrepareRequest{Actor: backupActor(c)}
	if strings.HasPrefix(c.ContentType(), "multipart/") {
		if err := readRestoreUpload(c, m, &req); err != nil {
			if req.UploadPath != "" {
				_ = os.Remove(req.UploadPath)
			}
			return nil, err
		}
	} else {
		var f restorePrepareForm
		if err := c.ShouldBindJSON(&f); err != nil {
			return nil, err
		}
		req.Name, req.SHA256, req.Password, req.RecoveryKey = f.Name, f.SHA256, f.Password, f.RecoveryKey
	}
	res, err := m.Prepare(c.Request.Context(), req)
	if err != nil {
		return nil, err
	}
	requires, err := restoreRequirements(c)
	if err != nil {
		return nil, err
	}
	noStore(c)
	return &RestorePrepareResponse{PrepareResult: res, ApplyRequires: requires}, nil
}

// readRestoreUpload streams the multipart upload to a temporary file below
// the data directory, without buffering it in memory.
func readRestoreUpload(c *gin.Context, m *backup.Manager, req *backup.PrepareRequest) error {
	mr, err := c.Request.MultipartReader()
	if err != nil {
		return err
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch part.FormName() {
		case "file":
			if req.UploadPath != "" {
				return errors.New("only one file can be uploaded")
			}
			f, err := m.UploadTempFile()
			if err != nil {
				return err
			}
			req.UploadPath = f.Name()
			n, err := io.Copy(f, io.LimitReader(part, m.MaxBackupBytes()+1))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			if n > m.MaxBackupBytes() {
				return backup.ErrTooLarge
			}
		case "password", "recovery_key", "name", "sha256":
			v, err := io.ReadAll(io.LimitReader(part, 4096))
			if err != nil {
				return err
			}
			switch part.FormName() {
			case "password":
				req.Password = string(v)
			case "recovery_key":
				req.RecoveryKey = string(v)
			}
		}
		_ = part.Close()
	}
	if req.UploadPath == "" {
		return errors.New("no backup file uploaded")
	}
	return nil
}

func currentLoginUser(c *gin.Context) (*model.User, *model.JWTSession, error) {
	actor := backupActor(c)
	var user model.User
	if err := singleton.DB.Select("id", "password", "reject_password").First(&user, actor.UserID).Error; err != nil {
		return nil, nil, newGormError("%v", err)
	}
	var sess model.JWTSession
	if actor.SessionID == "" {
		return &user, nil, nil
	}
	if err := singleton.DB.First(&sess, "key_id = ?", actor.SessionID).Error; err != nil {
		return &user, nil, nil
	}
	return &user, &sess, nil
}

func restoreRequirements(c *gin.Context) (restoreApplyRequires, error) {
	user, sess, err := currentLoginUser(c)
	if err != nil {
		return restoreApplyRequires{}, err
	}
	if !user.RejectPassword {
		return restoreApplyRequires{LoginPassword: true}, nil
	}
	return restoreApplyRequires{ReauthNeeded: sess == nil || time.Since(sess.CreatedAt) > reauthWindow}, nil
}

// restoreReauth verifies the administrator again before a restore is
// applied: the login password, or for accounts that sign in only through
// OAuth (no usable password), a login within the last reauthWindow.
func restoreReauth(c *gin.Context, loginPassword string) func() error {
	return func() error {
		user, sess, err := currentLoginUser(c)
		if err != nil {
			return err
		}
		if !user.RejectPassword {
			if loginPassword == "" {
				return errLoginPasswordNeed
			}
			if bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(loginPassword)) != nil {
				return errLoginPasswordWrong
			}
			return nil
		}
		if sess == nil || time.Since(sess.CreatedAt) > reauthWindow {
			return errReauthNeeded
		}
		return nil
	}
}

type restoreApplyForm struct {
	RestoreToken  string   `json:"restore_token"`
	Password      string   `json:"password"`
	RecoveryKey   string   `json:"recovery_key"`
	Confirm       string   `json:"confirm"`
	LoginPassword string   `json:"login_password"`
	Acknowledged  []string `json:"acknowledged"`
}

// Apply a staged restore
// @Summary Apply a staged restore
// @Security BearerAuth
// @Schemes
// @Description Re-verifies the administrator (backup password or recovery key, confirmation text = last 6 characters of the backup name, login password or a recent login) and the staged files, takes a pre-restore backup and restarts the dashboard, which swaps the data in before opening the database.
// @Tags admin required
// @Accept json
// @param request body restoreApplyForm true "apply"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /backup/restore/apply [post]
func applyRestore(c *gin.Context) (any, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	var f restoreApplyForm
	if err := c.ShouldBindJSON(&f); err != nil {
		return nil, err
	}
	return nil, m.Apply(c.Request.Context(), backup.ApplyRequest{
		Token:        f.RestoreToken,
		Password:     f.Password,
		RecoveryKey:  f.RecoveryKey,
		Confirm:      f.Confirm,
		Acknowledged: f.Acknowledged,
		Reauth:       restoreReauth(c, f.LoginPassword),
		Actor:        backupActor(c),
	})
}

// Cancel a staged restore
// @Summary Cancel a staged restore
// @Security BearerAuth
// @Schemes
// @Description Discards the staged restore of this token.
// @Tags admin required
// @Param token path string true "restore token"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /backup/restore/{token} [delete]
func cancelRestore(c *gin.Context) (any, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	m.Cancel(c.Param("token"), backupActor(c))
	return nil, nil
}

// Get last restore result
// @Summary Get last restore result
// @Security BearerAuth
// @Schemes
// @Description Result of the last restore (null if none).
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[backup.RestoreResult]
// @Router /backup/restore/status [get]
func getRestoreStatus(c *gin.Context) (*backup.RestoreResult, error) {
	m, err := backupManager()
	if err != nil {
		return nil, err
	}
	return m.RestoreStatus()
}

func noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
}
