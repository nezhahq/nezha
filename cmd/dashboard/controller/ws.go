package controller

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-uuid"
	"golang.org/x/sync/singleflight"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

var upgrader *websocket.Upgrader

func InitUpgrader() {
	var checkOrigin func(r *http.Request) bool

	// Allow CORS from loopback addresses in debug mode
	if singleton.Conf.Debug {
		checkOrigin = func(r *http.Request) bool {
			if checkSameOrigin(r) {
				return true
			}
			hostAddr := r.Host
			host, _, err := net.SplitHostPort(hostAddr)
			if err != nil {
				return false
			}
			if ip := net.ParseIP(host); ip != nil {
				if ip.IsLoopback() {
					return true
				}
			} else {
				// Handle domains like "localhost"
				ip, err := net.LookupHost(host)
				if err != nil || len(ip) == 0 {
					return false
				}
				if netIP := net.ParseIP(ip[0]); netIP != nil && netIP.IsLoopback() {
					return true
				}
			}
			return false
		}
	}

	upgrader = &websocket.Upgrader{
		ReadBufferSize:  32768,
		WriteBufferSize: 32768,
		CheckOrigin:     checkOrigin,
	}
}

func equalASCIIFold(s, t string) bool {
	for s != "" && t != "" {
		sr, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		tr, size := utf8.DecodeRuneInString(t)
		t = t[size:]
		if sr == tr {
			continue
		}
		if 'A' <= sr && sr <= 'Z' {
			sr = sr + 'a' - 'A'
		}
		if 'A' <= tr && tr <= 'Z' {
			tr = tr + 'a' - 'A'
		}
		if sr != tr {
			return false
		}
	}
	return s == t
}

func checkSameOrigin(r *http.Request) bool {
	origin := r.Header["Origin"]
	if len(origin) == 0 {
		return true
	}
	u, err := url.Parse(origin[0])
	if err != nil {
		return false
	}
	return equalASCIIFold(u.Host, r.Host)
}

// Websocket server stream
// @Summary Websocket server stream
// @tags common
// @Schemes
// @Description Websocket server stream
// @security BearerAuth
// @Produce json
// @Success 200 {object} model.StreamServerData
// @Router /ws/server [get]
func serverStream(c *gin.Context) (any, error) {
	connId, err := uuid.GenerateUUID()
	if err != nil {
		return nil, newWsError("%v", err)
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return nil, newWsError("%v", err)
	}
	defer conn.Close()

	deregisterPAT := registerPATConnection(c, func() { _ = conn.Close() })
	defer deregisterPAT()

	userIp := c.GetString(model.CtxKeyRealIPStr)
	if userIp == "" {
		userIp = c.RemoteIP()
	}

	u, isMember := c.Get(model.CtxKeyAuthorizedUser)
	var (
		userId  uint64
		isAdmin bool
	)
	if isMember {
		user := u.(*model.User)
		userId = user.ID
		isAdmin = user.Role.IsAdmin()
	}
	patAccessor, patCacheKey := patStreamContext(c)

	singleton.AddOnlineUser(connId, &model.OnlineUser{
		UserID:      userId,
		IP:          userIp,
		ConnectedAt: time.Now(),
		Conn:        conn,
	})
	defer singleton.RemoveOnlineUser(connId)

	count := 0
	for {
		stat, err := getServerStat(count == 0, userId, isAdmin, patAccessor, patCacheKey)
		if err != nil {
			// A malformed sample must not spin a core per connected viewer.
			time.Sleep(2 * time.Second)
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil {
				break
			}
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, stat); err != nil {
			break
		}
		count += 1
		if count%4 == 0 {
			err = conn.WriteMessage(websocket.PingMessage, []byte{})
			if err != nil {
				break
			}
		}
		time.Sleep(time.Second * 2)
	}
	return nil, newWsError("")
}

var requestGroup singleflight.Group

// getServerStat returns the websocket frame the viewer is allowed to see.
// The cache key must include the viewer's identity because the projection
// depends on per-server ownership: prior to GHSA-hvv7-hfrh-7gxj this function
// used a single isMember flag and leaked HideForGuest servers plus full Host
// (PlatformVersion, agent Version, GPU) to every authenticated user.
//
// patCacheKey distinguishes PATs with disjoint server_ids whitelists so two
// limited tokens for the same user do not share a singleflight projection.
func getServerStat(withPublicNote bool, viewerUserID uint64, viewerIsAdmin bool, pat model.APITokenAccessor, patCacheKey string) ([]byte, error) {
	cacheKey := fmt.Sprintf("serverStats::%t::%t::%d::%s", withPublicNote, viewerIsAdmin, viewerUserID, patCacheKey)
	v, err, _ := requestGroup.Do(cacheKey, func() (any, error) {
		servers := filterServersForViewer(
			singleton.ServerShared.GetSortedList(),
			viewerUserID, viewerIsAdmin, withPublicNote, pat,
		)
		return json.Marshal(model.StreamServerData{
			Now:     time.Now().Unix() * 1000,
			Online:  singleton.GetOnlineUserCount(),
			Servers: servers,
		})
	})

	return v.([]byte), err
}

// patStreamContext extracts the PAT accessor + a deterministic cache key
// fragment for the singleflight projection. Returns (nil, "jwt") for JWT
// requests so two callers from the same user collapse onto one frame.
func patStreamContext(c *gin.Context) (model.APITokenAccessor, string) {
	tok := APITokenFromContext(c)
	if tok == nil {
		return nil, "jwt"
	}
	ids := tok.ServerIDs()
	slices.Sort(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(id, 10))
	}
	return tok, fmt.Sprintf("pat:%d:%s", tok.ID, strings.Join(parts, ","))
}

// filterServersForViewer projects the global server list down to what a single
// viewer is allowed to see. The rules are:
//   - HideForGuest servers are visible only to their owner and to admins.
//   - Non-owner / non-admin viewers (including authenticated members) get
//     Host.Filter() output, which drops PlatformVersion and agent Version.
//   - Admins are unconstrained.
//   - A non-nil pat whitelist narrows visibility further; servers outside its
//     allow-list are dropped even from admins/owners (a PAT scoped to a
//     subset must never widen via its caller's role).
//
// viewerUserID == 0 represents an unauthenticated guest.
func filterServersForViewer(servers []*model.Server, viewerUserID uint64, viewerIsAdmin bool, withPublicNote bool, pat model.APITokenAccessor) []model.StreamServer {
	out := make([]model.StreamServer, 0, len(servers))
	for _, server := range servers {
		runtime := server.RuntimeSnapshot()
		if pat != nil && !pat.CanAccessServer(server.ID) {
			continue
		}
		isOwnerOrAdmin := viewerIsAdmin || (viewerUserID != 0 && server.GetUserID() == viewerUserID)
		if server.HideForGuest && !isOwnerOrAdmin {
			continue
		}
		var countryCode string
		if server.GeoIP != nil {
			countryCode = server.GeoIP.CountryCode
		}
		publicHost := runtime.Host
		if publicHost != nil && !isOwnerOrAdmin {
			publicHost = publicHost.Filter()
		}
		out = append(out, model.StreamServer{
			ID:           server.ID,
			Name:         server.Name,
			PublicNote:   utils.IfOr(withPublicNote, server.PublicNote, ""),
			DisplayIndex: server.DisplayIndex,
			Host:         publicHost,
			State:        runtime.State,
			CountryCode:  countryCode,
			LastActive:   runtime.LastActive,
		})
	}
	return out
}
