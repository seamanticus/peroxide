package imap

import (
	"errors"
	"sync"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

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
