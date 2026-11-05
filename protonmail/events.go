package protonmail

import (
	"encoding/json"
	"net/http"
)

type EventRefresh int

const (
	EventRefreshMail EventRefresh = 1 << iota
	EventRefreshContacts
)

type Event struct {
	ID string `json:"EventID"`
	// More is 1 when more events follow this page: ask for the events after
	// ID again before waiting.
	More     int
	Refresh  EventRefresh
	Messages []*EventMessage
	Contacts []*EventContact
	//ContactEmails
	Labels []*EventLabel
	//User
	Addresses []*EventAddress
	//Members
	//Domains
	//Organization
	MessageCounts []*MessageCount
	//ConversationCounts
	//UsedSpace
	Notices []string
}

type EventAction int

const (
	EventDelete EventAction = iota
	EventCreate
	EventUpdate

	// For messages
	EventUpdateFlags
)

type EventMessage struct {
	ID     string
	Action EventAction

	// Only populated for EventCreate
	Created *Message
	// Only populated for EventUpdate or EventUpdateFlags
	Updated *EventMessageUpdate
}

type EventMessageUpdate struct {
	Unread       *int
	Type         *MessageType
	Time         Timestamp
	IsReplied    *int
	IsRepliedAll *int
	IsForwarded  *int

	// Only populated for EventUpdateFlags
	LabelIDs        []string
	LabelIDsAdded   []string
	LabelIDsRemoved []string
}

func buildLabelsSet(labelIDs []string) map[string]struct{} {
	set := make(map[string]struct{}, len(labelIDs))
	for _, labelID := range labelIDs {
		set[labelID] = struct{}{}
	}
	return set
}

func (update *EventMessageUpdate) DiffLabelIDs(current []string) (added, removed []string) {
	if update.LabelIDsAdded != nil && update.LabelIDsRemoved != nil {
		return update.LabelIDsAdded, update.LabelIDsRemoved
	}
	if update.LabelIDs == nil {
		return
	}

	currentSet := buildLabelsSet(current)
	updateSet := buildLabelsSet(update.LabelIDs)
	for labelID := range currentSet {
		if _, ok := updateSet[labelID]; !ok {
			removed = append(removed, labelID)
		}
	}
	for labelID := range updateSet {
		if _, ok := currentSet[labelID]; !ok {
			added = append(added, labelID)
		}
	}
	return
}

func (update *EventMessageUpdate) Patch(msg *Message) {
	// Proton's updates always carry it; peroxide's own may leave it out.
	if update.Time != 0 {
		msg.Time = update.Time
	}
	if update.Unread != nil {
		msg.Unread = *update.Unread
	}
	if update.Type != nil {
		msg.Type = *update.Type
	}
	if update.IsReplied != nil {
		msg.IsReplied = *update.IsReplied
	}
	if update.IsRepliedAll != nil {
		msg.IsRepliedAll = *update.IsRepliedAll
	}
	if update.IsForwarded != nil {
		msg.IsForwarded = *update.IsForwarded
	}

	if update.LabelIDs != nil {
		msg.LabelIDs = update.LabelIDs
	} else if update.LabelIDsAdded != nil && update.LabelIDsRemoved != nil {
		set := buildLabelsSet(msg.LabelIDs)
		for _, labelID := range update.LabelIDsAdded {
			set[labelID] = struct{}{}
		}
		for _, labelID := range update.LabelIDsRemoved {
			delete(set, labelID)
		}
		msg.LabelIDs = make([]string, 0, len(set))
		for labelID := range set {
			msg.LabelIDs = append(msg.LabelIDs, labelID)
		}
	}
}

type rawEventMessage struct {
	ID      string
	Action  EventAction
	Message json.RawMessage `json:",omitempty"`
}

func (em *EventMessage) UnmarshalJSON(b []byte) error {
	var raw rawEventMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	em.ID = raw.ID
	em.Action = raw.Action
	switch raw.Action {
	case EventCreate:
		em.Created = new(Message)
		return json.Unmarshal(raw.Message, em.Created)
	case EventUpdate, EventUpdateFlags:
		em.Updated = new(EventMessageUpdate)
		return json.Unmarshal(raw.Message, em.Updated)
	case EventDelete:
		return nil // Carries no message.
	}
	// An action added by Proton since: kept for the caller to report.
	return nil
}

type EventContact struct {
	ID      string
	Action  EventAction
	Contact *Contact
}

// EventLabel is a folder or label created, updated or deleted. Label is nil
// for EventDelete.
type EventLabel struct {
	ID     string
	Action EventAction
	Label  *Label
}

// EventAddress is an address of the account created, updated or deleted.
// Address is nil for EventDelete, whose ID is the address ID.
type EventAddress struct {
	ID      string
	Action  EventAction
	Address *Address
}

func (c *Client) GetEvent(last string) (*Event, error) {
	if last == "" {
		last = "latest"
	}

	req, err := c.newRequest(http.MethodGet, "/events/"+last, nil)
	if err != nil {
		return nil, err
	}

	var respData struct {
		resp
		*Event
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}

	return respData.Event, nil
}

// GetLatestEventID returns the ID of the latest event: polling the events
// after it gives every change made since this call.
func (c *Client) GetLatestEventID() (string, error) {
	event, err := c.GetEvent("latest")
	if err != nil {
		return "", err
	}
	return event.ID, nil
}
