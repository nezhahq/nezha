package controller

import (
	"context"
	"log"
	"strings"

	jwt "github.com/golang-jwt/jwt/v4"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	ginjwt "github.com/appleboy/gin-jwt/v2"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

// dashboardCredentialJWTParser verifies that a JWT was signed by this
// dashboard. It is derived from the auth middleware created in routers() and
// exists only for dashboard-credential classification; it must never
// authenticate dashboard requests on its own.
var dashboardCredentialJWTParser *ginjwt.GinJWTMiddleware

// newDashboardCredentialJWTParser derives a signature-only parser from the
// dashboard auth middleware. It keeps the exact key value and the pinned HS256
// signing algorithm of initParams() — there is no second read of
// singleton.Conf.JWTSecretKey — and disables claims validation so expiry does
// not change the verdict: an expired token is still proof the dashboard issued
// it, and keeping expired dashboard tokens out of the NAT tunnel prevents a
// careless backend from treating "any JWT" as its own credential. The live
// auth middleware keeps full claims validation; dashboard authentication is
// not relaxed by this parser in any way.
func newDashboardCredentialJWTParser(authMiddleware *ginjwt.GinJWTMiddleware) *ginjwt.GinJWTMiddleware {
	parser := *authMiddleware
	parser.ParseOptions = []jwt.ParserOption{jwt.WithoutClaimsValidation()}
	return &parser
}

// IsDashboardCredential reports whether an Authorization header value was
// issued by this dashboard: a panel-signed JWT, PAT, or reserved MCP OAuth credential.
//
// NAT ingress uses this to enforce the tunnel credential invariant: values
// classified as dashboard-issued are stripped before the request is tunneled
// to the agent, while foreign credentials are forwarded untouched because the
// NAT backend may authenticate with credentials of its own.
//
// Decision policy mirrors the dashboard's own auth middlewares: only the
// case-sensitive "Bearer " scheme is considered; the value after the scheme is
// classified by IsDashboardCredentialValue, which is also the entry point for
// the panel's scheme-less TokenLookup channels. Only a deterministic proof
// that the value was not issued by this dashboard (scheme mismatch, unknown
// PAT hash, failed signature verification) returns false.
func IsDashboardCredential(authz string) bool {
	credential := utils.BearerCredential(authz)
	if credential == "" {
		return false
	}
	return IsDashboardCredentialValue(credential)
}

// IsDashboardCredentialValue reports whether a raw credential value — the part
// after the "Bearer " scheme, exactly as the panel's other TokenLookup
// channels carry it (the ?token= query parameter and the nz-jwt cookie, see
// initParams) — was issued by this dashboard: a panel-signed JWT or a panel
// API token (PAT), or belongs to the reserved MCP OAuth credential namespace.
//
// Failure policy is identical to IsDashboardCredential: whenever the
// classification cannot be completed (parser or config not ready, database
// failure) the value is conservatively reported as a dashboard credential so
// the caller strips it; only a deterministic proof that the value was not
// issued by this dashboard (unknown PAT hash, failed signature verification)
// returns false.
func IsDashboardCredentialValue(value string) bool {
	classified := ClassifyDashboardCredentialValues(context.Background(), []string{value})
	if len(classified) != 1 {
		return true
	}
	return classified[0]
}

// ClassifyDashboardCredentialValues classifies a bounded request batch. JWTs
// are checked independently, MCP OAuth prefixes are always stripped, and
// all PAT-shaped values are hashed and
// resolved with one silent IN query. This prevents attacker-controlled NAT
// query parameters or cookies from amplifying one HTTP request into thousands
// of SQLite queries and record-not-found log entries.
func ClassifyDashboardCredentialValues(ctx context.Context, values []string) []bool {
	if ctx == nil {
		ctx = context.Background()
	}
	results := make([]bool, len(values))
	patIndexes := make(map[string][]int)
	for i, value := range values {
		plaintext := strings.TrimSpace(value)
		// MCP OAuth owns these reserved credential namespaces. Strip them even
		// after rotation/revocation removes their stored hashes: forwarding a
		// stale dashboard credential to a NAT tenant must never become allowed.
		if strings.HasPrefix(plaintext, mcpOAuthAccessPrefix) || strings.HasPrefix(plaintext, mcpOAuthRefreshPrefix) {
			results[i] = true
			continue
		}
		if strings.HasPrefix(plaintext, model.APITokenPrefix) {
			hash := model.HashAPIToken(plaintext)
			patIndexes[hash] = append(patIndexes[hash], i)
			continue
		}
		results[i] = isDashboardSignedJWT(plaintext)
	}
	if len(patIndexes) == 0 {
		return results
	}
	if singleton.DB == nil {
		logDashboardCredentialUnclassifiable("api token lookup unavailable")
		for _, indexes := range patIndexes {
			for _, i := range indexes {
				results[i] = true
			}
		}
		return results
	}

	hashes := make([]string, 0, len(patIndexes))
	for hash := range patIndexes {
		hashes = append(hashes, hash)
	}
	type tokenHashRow struct {
		TokenHash string
	}
	var rows []tokenHashRow
	err := singleton.DB.WithContext(ctx).
		Session(&gorm.Session{Logger: logger.Discard}).
		Model(&model.APIToken{}).
		Select("token_hash").
		Where("token_hash IN ?", hashes).
		Find(&rows).Error
	if err != nil {
		logDashboardCredentialUnclassifiable("api token lookup failed")
		for _, indexes := range patIndexes {
			for _, i := range indexes {
				results[i] = true
			}
		}
		return results
	}
	for _, row := range rows {
		for _, i := range patIndexes[row.TokenHash] {
			results[i] = true
		}
	}
	return results
}

// isDashboardSignedJWT verifies the token signature with the dashboard's
// pinned algorithm and key. Claims validation is disabled on purpose (see
// newDashboardCredentialJWTParser): expiry does not change who issued the
// token.
func isDashboardSignedJWT(token string) bool {
	if dashboardCredentialJWTParser == nil {
		// routers() has not run yet (startup incomplete): fail closed.
		logDashboardCredentialUnclassifiable("dashboard credential parser unavailable")
		return true
	}
	_, err := dashboardCredentialJWTParser.ParseTokenString(token)
	return err == nil
}

// logDashboardCredentialUnclassifiable records a classification failure. The
// credential value itself is never included in the message.
func logDashboardCredentialUnclassifiable(reason string) {
	log.Printf("NEZHA>> NAT ingress: credential value could not be classified (%s); stripping it as a precaution", reason)
}
