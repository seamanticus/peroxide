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
)

var errNotYetImplemented = errors.New("not yet implemented")

// appendLimit is the largest message APPEND accepts, in octets: Proton refuses
// messages over 50 MB (go-proton-api ErrImportSizeExceeded; APPEND makes a
// draft today, TODO section 3 moves it to the import API).
const appendLimit = 50 << 20

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
	updates       chan imapbackend.Update
	// Whether LIST shows All Mail, as proton-bridge's ShowAllMail setting.
	showAllMail bool

	sync.Mutex // protects everything below

	users map[string]*user
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

// disconnect ends every connection logged in as u, as proton-bridge removes
// its IMAP user. go-imap's conn.Close logs the user out, and serveConn closes
// the connection again when it ends, which would log it out twice: so the
// connection's deadline is set instead, which fails its read and lets go-imap
// close it once. Called without be's lock, which Logout takes while go-imap
// holds the server's.
func (be *backend) disconnect(u *user) {
	be.Lock()
	s := be.server
	be.Unlock()
	if s == nil {
		return
	}
	s.ForEachConn(func(c imapserver.Conn) {
		if cu, ok := c.Context().User.(*user); !ok || cu != u {
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
	c, privateKeys, err := be.sessions.Auth(username, password)
	if err != nil {
		return nil, err
	}

	return getUser(be, username, c, privateKeys)
}

func (be *backend) Updates() <-chan imapbackend.Update {
	return be.updates
}

// New returns the IMAP backend. showAllMail false leaves All Mail out of LIST
// (user.ListMailboxes).
func New(sessions *auth.Manager, eventsManager *events.Manager, showAllMail bool) imapbackend.Backend {
	return &backend{
		sessions:      sessions,
		eventsManager: eventsManager,
		showAllMail:   showAllMail,
		updates:       make(chan imapbackend.Update, 50),
		users:         make(map[string]*user),
	}
}
