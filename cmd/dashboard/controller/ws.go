package controller

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
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

	userIp := c.GetString(model.CtxKeyRealIPStr)
	if userIp == "" {
		userIp = c.RemoteIP()
	}

	singleton.AddOnlineUser(connId, &model.OnlineUser{
		IP:          userIp,
		ConnectedAt: time.Now(),
		Conn:        conn,
	})
	done := make(chan struct{})
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				close(done)
				singleton.RemoveOnlineUser(connId)
				_ = conn.Close()
				return
			}
		}
	}()

	count := 0
	for {
		select {
		case <-done:
			return nil, newWsError("connection closed")
		default:
			stat, err := getServerStat(c, count == 0)
			if err != nil {
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage, stat); err != nil {
				singleton.RemoveOnlineUser(connId)
				return nil, newWsError("write error: %v", err)
			}
			count += 1
			if count%4 == 0 {
				err = conn.WriteMessage(websocket.PingMessage, []byte{})
				if err != nil {
					singleton.RemoveOnlineUser(connId)
					return nil, newWsError("ping error: %v", err)
				}
			}
			time.Sleep(time.Second * 2)
		}
	}
}

var requestGroup singleflight.Group

func getServerStat(c *gin.Context, withPublicNote bool) ([]byte, error) {
	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	authorized := isMember // TODO || isViewPasswordVerfied
	v, err, _ := requestGroup.Do(fmt.Sprintf("serverStats::%t", authorized), func() (interface{}, error) {
		singleton.SortedServerLock.RLock()
		defer singleton.SortedServerLock.RUnlock()

		var serverList []*model.Server
		if authorized {
			serverList = singleton.SortedServerList
		} else {
			serverList = singleton.SortedServerListForGuest
		}

		servers := make([]model.StreamServer, 0, len(serverList))
		for _, server := range serverList {
			var countryCode string
			if server.GeoIP != nil {
				countryCode = server.GeoIP.CountryCode
			}
			servers = append(servers, model.StreamServer{
				ID:           server.ID,
				Name:         server.Name,
				PublicNote:   utils.IfOr(withPublicNote, server.PublicNote, ""),
				DisplayIndex: server.DisplayIndex,
				Host:         utils.IfOr(authorized, server.Host, server.Host.Filter()),
				State:        server.State,
				CountryCode:  countryCode,
				LastActive:   server.LastActive,
			})
		}

		return utils.Json.Marshal(model.StreamServerData{
			Now:     time.Now().Unix() * 1000,
			Online:  singleton.GetOnlineUserCount(),
			Servers: servers,
		})
	})

	return v.([]byte), err
}
