package imap

import (
	"container/list"
	"sync"

	"github.com/seamanticus/peroxide/protonmail"
)

// messageCacheSize is how many full messages a user keeps in memory. Clients
// fetch a message's parts in several commands, and each needs the full
// message, so caching the last few saves most requests.
const messageCacheSize = 32

// messageCache holds the most recently fetched full messages: encrypted
// bodies and attachment metadata, as returned by the API. They are kept for
// their contents, which don't change; flags and labels come from the local
// database.
type messageCache struct {
	mu       sync.Mutex
	order    *list.List // of message IDs, most recently used first
	messages map[string]*list.Element
	values   map[string]*protonmail.Message
}

func newMessageCache() *messageCache {
	return &messageCache{
		order:    list.New(),
		messages: make(map[string]*list.Element),
		values:   make(map[string]*protonmail.Message),
	}
}

func (c *messageCache) get(id string) (*protonmail.Message, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.messages[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return c.values[id], true
}

func (c *messageCache) put(msg *protonmail.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.messages[msg.ID]; ok {
		c.order.MoveToFront(e)
		c.values[msg.ID] = msg
		return
	}
	c.messages[msg.ID] = c.order.PushFront(msg.ID)
	c.values[msg.ID] = msg
	for c.order.Len() > messageCacheSize {
		oldest := c.order.Back()
		id := c.order.Remove(oldest).(string)
		delete(c.messages, id)
		delete(c.values, id)
	}
}

func (c *messageCache) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.messages[id]; ok {
		c.order.Remove(e)
		delete(c.messages, id)
		delete(c.values, id)
	}
}

func (c *messageCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.order.Init()
	clear(c.messages)
	clear(c.values)
}

// fullMessage returns the message id with its body and attachments, from the
// cache if possible.
func (u *user) fullMessage(id string) (*protonmail.Message, error) {
	if msg, ok := u.messages.get(id); ok {
		return msg, nil
	}
	msg, err := u.c.GetMessage(id)
	if err != nil {
		return nil, err
	}
	u.messages.put(msg)
	return msg, nil
}
