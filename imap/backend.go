package imap

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	imapserver "github.com/emersion/go-imap/server"

	"github.com/seamanticus/peroxide/auth"
	"github.com/seamanticus/peroxide/events"
	"github.com/seamanticus/peroxide/protonmail"
	"github.com/seamanticus/peroxide/sendrecord"
)

var errNotYetImplemented = errors.New("not yet implemented")

// appendLimit is the largest message APPEND accepts, in octets: the import
// API's limit, which it applies once the message is encrypted (Proton's 50 MB
// of attachments, base64-encoded). Encrypting a message keeps its size, give
// or take, unless it is encrypted whole (armored, a third larger): then
// CreateMessage refuses it with TOOBIG after encrypting it.
const appendLimit = protonmail.MaxImportSize

// MaxLiteralSize is the largest literal the server must accept: an APPENDed
// message, the largest there is. Set it as go-imap's Server.MaxLiteralSize,
// which reads every literal whole into memory before any command sees it:
// without the cap, a client could make peroxide allocate any size.
const MaxLiteralSize = appendLimit

// CreateMessageLimit makes go-imap announce APPENDLIMIT=appendLimit, the same
// for every mailbox (RFC 7889 section 2).
func (u *user) CreateMessageLimit() *uint32 {
	return new(uint32(appendLimit))
}

var _ imapbackend.AppendLimitUser = (*user)(nil) // proof: compile-time

type backend struct {
	sessions      *auth.Manager
	eventsManager *events.Manager
	// The messages the SMTP server sent, which APPEND finds instead of
	// importing them again.
	sends   *sendrecord.Recorder
	updates chan imapbackend.Update
	// Whether LIST shows All Mail, as proton-bridge's ShowAllMail setting.
	showAllMail bool

	sync.Mutex // protects everything below

	users map[string]*user
	// The connections logged in, by their remote and local addresses
	// (connKey), to the user they logged in as: backend.disconnect matches
	// connections by them, as reading go-imap's Context().User from another
	// goroutine races with the connection writing it.
	logins map[string]*user
	// The server serving this backend, learnt from its first connection
	// (Extension.NewConn): go-imap gives the backend no other way to reach
	// the connections.
	server *imapserver.Server
}

func (be *backend) setServer(s *imapserver.Server) {
	be.Lock()
	defer be.Unlock()
	be.server = s
}

// connKey identifies a connection by its addresses, which go-imap gives to
// Login (ConnInfo) and to ForEachConn (Conn.Info) alike.
func connKey(info *imap.ConnInfo) string {
	if info == nil || info.RemoteAddr == nil || info.LocalAddr == nil {
		return ""
	}
	return info.RemoteAddr.Network() + " " + info.RemoteAddr.String() + " " + info.LocalAddr.String()
}

// disconnect ends every connection logged in as u, as proton-bridge removes
// its IMAP user. go-imap's conn.Close logs the user out, and serveConn closes
// the connection again when it ends, which would log it out twice: so the
// connection's deadline is set instead, which fails its read and lets go-imap
// close it once. Called without be's lock, which Logout takes while go-imap
// holds the server's.
func (be *backend) disconnect(u *user) {
	be.Lock()
	s := be.server
	keys := make(map[string]struct{})
	for k, lu := range be.logins {
		if lu == u {
			keys[k] = struct{}{}
		}
	}
	be.Unlock()
	if s == nil || len(keys) == 0 {
		return
	}
	s.ForEachConn(func(c imapserver.Conn) {
		if _, ok := keys[connKey(c.Info())]; !ok {
			return
		}
		d, ok := c.(interface{ SetDeadline(time.Time) error })
		if !ok {
			log.Printf("cannot disconnect a client of %q: %T has no SetDeadline", u.username, c)
			return
		}
		if err := d.SetDeadline(time.Unix(1, 0)); err != nil {
			log.Printf("cannot disconnect a client of %q: %v", u.username, err)
		}
	})
}

func (be *backend) Login(info *imap.ConnInfo, username, password string) (imapbackend.User, error) {
	c, privateKeys, storeKey, err := be.sessions.Auth(username, password)
	if err != nil {
		return nil, err
	}

	u, err := getUser(be, username, c, privateKeys, storeKey)
	if err != nil {
		return nil, err
	}
	if k := connKey(info); k != "" {
		be.Lock()
		be.logins[k] = u
		be.Unlock()
	}
	return u, nil
}

func (be *backend) Updates() <-chan imapbackend.Update {
	return be.updates
}

// New returns the IMAP backend. sends are the messages the SMTP server sent,
// shared with it (sendrecord). showAllMail false leaves All Mail out of LIST
// (user.ListMailboxes).
func New(sessions *auth.Manager, eventsManager *events.Manager, sends *sendrecord.Recorder, showAllMail bool) imapbackend.Backend {
	if sends == nil {
		panic("invariant: imap.New needs a sendrecord.Recorder")
	}
	return &backend{
		sessions:      sessions,
		eventsManager: eventsManager,
		sends:         sends,
		showAllMail:   showAllMail,
		updates:       make(chan imapbackend.Update, 50),
		users:         make(map[string]*user),
		logins:        make(map[string]*user),
	}
}
