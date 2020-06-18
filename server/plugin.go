package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
	"github.com/pkg/errors"
)

const (
	// WSEventRefresh is the WebSocket event for refreshing the Todo list
	WSEventRefresh = "refresh"
)

// ListManager represents the logic on the lists
type ListManager interface {
	// AddIssue adds a todo to userID's myList with the message
	AddIssue(userID, message, postID string) error
	// SendIssue sends the todo with the message from senderID to receiverID and returns the receiver's issueID
	SendIssue(senderID, receiverID, message, postID string) (string, error)
	// GetIssueList gets the todos on listID for userID
	GetIssueList(userID, listID string) ([]*ExtendedIssue, error)
	// CompleteIssue completes the todo issueID for userID, and returns the issue and the foreign ID if any
	CompleteIssue(userID, issueID string) (issue *Issue, foreignID string, err error)
	// AcceptIssue moves one the todo issueID of userID from inbox to myList, and returns the message and the foreignUserID if any
	AcceptIssue(userID, issueID string) (todoMessage string, foreignUserID string, err error)
	// RemoveIssue removes the todo issueID for userID and returns the issue, the foreign ID if any and whether the user sent the todo to someone else
	RemoveIssue(userID, issueID string) (issue *Issue, foreignID string, isSender bool, err error)
	// PopIssue the first element of myList for userID and returns the issue and the foreign ID if any
	PopIssue(userID string) (issue *Issue, foreignID string, err error)
	// BumpIssue moves a issueID sent by userID to the top of its receiver inbox list
	BumpIssue(userID string, issueID string) (todoMessage string, receiver string, foreignIssueID string, err error)
	// GetUserName returns the readable username from userID
	GetUserName(userID string) string
}

// Plugin implements the interface expected by the Mattermost server to communicate between the server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	BotUserID string

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration

	listManager ListManager
}

func (p *Plugin) OnActivate() error {
	config := p.getConfiguration()
	if err := config.IsValid(); err != nil {
		return err
	}

	botID, err := p.Helpers.EnsureBot(&model.Bot{
		Username:    "todo",
		DisplayName: "Todo Bot",
		Description: "Created by the Todo plugin.",
	})
	if err != nil {
		return errors.Wrap(err, "failed to ensure todo bot")
	}
	p.BotUserID = botID

	p.listManager = NewListManager(p.API)

	return p.API.RegisterCommand(getCommand())
}

// ServeHTTP demonstrates a plugin that handles HTTP requests by greeting the world.
func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/add":
		p.handleAdd(w, r)
	case "/list":
		p.handleList(w, r)
	case "/remove":
		p.handleRemove(w, r)
	case "/complete":
		p.handleComplete(w, r)
	case "/accept":
		p.handleAccept(w, r)
	case "/bump":
		p.handleBump(w, r)
	default:
		http.NotFound(w, r)
	}
}

type addAPIRequest struct {
	Message string `json:"message"`
	SendTo  string `json:"send_to"`
	PostID  string `json:"post_id"`
}

func (p *Plugin) handleAdd(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var addRequest *addAPIRequest
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&addRequest)
	if err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	senderName := p.listManager.GetUserName(userID)

	if addRequest.SendTo == "" {
		err = p.listManager.AddIssue(userID, addRequest.Message, addRequest.PostID)
		if err != nil {
			p.API.LogError("Unable to add issue err=" + err.Error())
			p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to add issue", err)
			return
		}
		replyMessage := fmt.Sprintf("@%s attached a todo to this thread", senderName)
		p.postReplyIfNeeded(addRequest.PostID, replyMessage, addRequest.Message)
		return
	}

	receiver, appErr := p.API.GetUserByUsername(addRequest.SendTo)
	if appErr != nil {
		p.API.LogError("username not valid, err=" + appErr.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to find user", err)
		return
	}

	if receiver.Id == userID {
		err = p.listManager.AddIssue(userID, addRequest.Message, addRequest.PostID)
		if err != nil {
			p.API.LogError("Unable to add issue err=" + err.Error())
			p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to add issue", err)
			return
		}
		replyMessage := fmt.Sprintf("@%s attached a todo to this thread", senderName)
		p.postReplyIfNeeded(addRequest.PostID, replyMessage, addRequest.Message)
		return
	}

	issueID, err := p.listManager.SendIssue(userID, receiver.Id, addRequest.Message, addRequest.PostID)

	if err != nil {
		p.API.LogError("Unable to send issue err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to send issue", err)
		return
	}

	receiverMessage := fmt.Sprintf("You have received a new Todo from @%s", senderName)
	p.sendRefreshEvent(receiver.Id)
	p.PostBotCustomDM(receiver.Id, receiverMessage, addRequest.Message, issueID)

	replyMessage := fmt.Sprintf("@%s sent @%s a todo attached to this thread", senderName, addRequest.SendTo)
	p.postReplyIfNeeded(addRequest.PostID, replyMessage, addRequest.Message)
}

func (p *Plugin) postReplyIfNeeded(postID, message, todo string) {
	if postID != "" {
		err := p.ReplyPostBot(postID, message, todo)
		if err != nil {
			p.API.LogError(err.Error())
		}
	}
}

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	listInput := r.URL.Query().Get("list")
	listID := MyListKey
	switch listInput {
	case OutFlag:
		listID = OutListKey
	case InFlag:
		listID = InListKey
	}

	issues, err := p.listManager.GetIssueList(userID, listID)
	if err != nil {
		p.API.LogError("Unable to get issues for user err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to get issues for user", err)
		return
	}

	if len(issues) > 0 && r.URL.Query().Get("reminder") == "true" {
		var lastReminderAt int64
		lastReminderAt, err = p.getLastReminderTimeForUser(userID)
		if err != nil {
			p.API.LogError("Unable to send reminder err=" + err.Error())
			p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to send reminder", err)
			return
		}

		var timezone *time.Location
		offset, _ := strconv.Atoi(r.Header.Get("X-Timezone-Offset"))
		timezone = time.FixedZone("local", -60*offset)

		// Post reminder message if it's the next day and been more than an hour since the last post
		now := model.GetMillis()
		nt := time.Unix(now/1000, 0).In(timezone)
		lt := time.Unix(lastReminderAt/1000, 0).In(timezone)
		if nt.Sub(lt).Hours() >= 1 && (nt.Day() != lt.Day() || nt.Month() != lt.Month() || nt.Year() != lt.Year()) {
			p.PostBotDM(userID, "Daily Reminder:\n\n"+issuesListToString(issues))
			err = p.saveLastReminderTimeForUser(userID)
			if err != nil {
				p.API.LogError("Unable to save last reminder for user err=" + err.Error())
			}
		}
	}

	issuesJSON, err := json.Marshal(issues)
	if err != nil {
		p.API.LogError("Unable marhsal issues list to json err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable marhsal issues list to json", err)
		return
	}

	_, err = w.Write(issuesJSON)
	if err != nil {
		p.API.LogError("Unable to write json response err=" + err.Error())
	}
}

type acceptAPIRequest struct {
	ID string `json:"id"`
}

func (p *Plugin) handleAccept(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var acceptRequest *acceptAPIRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&acceptRequest); err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	todoMessage, sender, err := p.listManager.AcceptIssue(userID, acceptRequest.ID)

	if err != nil {
		p.API.LogError("Unable to accept issue err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to accept issue", err)
		return
	}

	userName := p.listManager.GetUserName(userID)

	message := fmt.Sprintf("@%s accepted a Todo you sent: %s", userName, todoMessage)
	p.sendRefreshEvent(sender)
	p.PostBotDM(sender, message)
}

type completeAPIRequest struct {
	ID string `json:"id"`
}

func (p *Plugin) handleComplete(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var completeRequest *completeAPIRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&completeRequest); err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	issue, foreignID, err := p.listManager.CompleteIssue(userID, completeRequest.ID)
	if err != nil {
		p.API.LogError("Unable to complete issue err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to complete issue", err)
		return
	}

	userName := p.listManager.GetUserName(userID)
	replyMessage := fmt.Sprintf("@%s completed a todo attached to this thread", userName)
	p.postReplyIfNeeded(issue.PostID, replyMessage, issue.Message)

	if foreignID == "" {
		return
	}

	message := fmt.Sprintf("@%s completed a Todo you sent: %s", userName, issue.Message)
	p.sendRefreshEvent(foreignID)
	p.PostBotDM(foreignID, message)
}

type removeAPIRequest struct {
	ID string `json:"id"`
}

func (p *Plugin) handleRemove(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var removeRequest *removeAPIRequest
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&removeRequest)
	if err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	issue, foreignID, isSender, err := p.listManager.RemoveIssue(userID, removeRequest.ID)
	if err != nil {
		p.API.LogError("Unable to remove issue, err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to remove issue", err)
		return
	}

	userName := p.listManager.GetUserName(userID)
	replyMessage := fmt.Sprintf("@%s removed a todo attached to this thread", userName)
	p.postReplyIfNeeded(issue.PostID, replyMessage, issue.Message)

	if foreignID == "" {
		return
	}

	message := fmt.Sprintf("@%s removed a Todo you received: %s", userName, issue.Message)
	if isSender {
		message = fmt.Sprintf("@%s declined a Todo you sent: %s", userName, issue.Message)
	}

	p.sendRefreshEvent(foreignID)
	p.PostBotDM(foreignID, message)
}

type bumpAPIRequest struct {
	ID string `json:"id"`
}

func (p *Plugin) handleBump(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var bumpRequest *bumpAPIRequest
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&bumpRequest)
	if err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	todoMessage, foreignUser, foreignIssueID, err := p.listManager.BumpIssue(userID, bumpRequest.ID)
	if err != nil {
		p.API.LogError("Unable to bump issue, err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to bump issue", err)
		return
	}

	if foreignUser == "" {
		return
	}

	userName := p.listManager.GetUserName(userID)

	message := fmt.Sprintf("@%s bumped a Todo you received.", userName)

	p.sendRefreshEvent(foreignUser)
	p.PostBotCustomDM(foreignUser, message, todoMessage, foreignIssueID)
}

func (p *Plugin) sendRefreshEvent(userID string) {
	p.API.PublishWebSocketEvent(
		WSEventRefresh,
		nil,
		&model.WebsocketBroadcast{UserId: userID},
	)
}

func (p *Plugin) handleErrorWithCode(w http.ResponseWriter, code int, errTitle string, err error) {
	w.WriteHeader(code)
	b, _ := json.Marshal(struct {
		Error   string `json:"error"`
		Details string `json:"details"`
	}{
		Error:   errTitle,
		Details: err.Error(),
	})
	_, _ = w.Write(b)
}
