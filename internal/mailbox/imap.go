// Package mailbox reconstructs receipts from confirmation e-mails.
//
// It is the fallback for stores whose own API is unreachable: an order
// confirmation almost always names the shop, the date and the amount, which is
// enough to categorize the spending even when the item lines are missing.
package mailbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"

	// Registers the character sets non-UTF-8 receipt mails still arrive in.
	_ "github.com/emersion/go-message/charset"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/secret"
)

// SecretID is the provider name the mailbox credentials are stored under.
const SecretID = "mail"

// DefaultMailbox is searched when no folder is configured.
const DefaultMailbox = "INBOX"

// dialTimeout bounds connecting and logging in.
const dialTimeout = 30 * time.Second

// ErrNotConfigured means no mailbox credentials are stored.
var ErrNotConfigured = errors.New("mailbox: not configured")

// Message is one fetched mail, reduced to what the parsers need.
type Message struct {
	UID     imap.UID
	ID      string
	From    string
	Subject string
	Date    time.Time
	Text    string
}

// Client fetches mail over IMAP. Connections are made per call: a receipt
// listing is rare and short, and a long-lived IMAP connection on a home server
// is mostly a source of timeouts.
type Client struct {
	store *secret.Store
}

// NewClient builds a mailbox client backed by the credential store.
func NewClient(store *secret.Store) *Client { return &Client{store: store} }

// Configured reports whether credentials are stored.
func (c *Client) Configured() bool {
	return c.store.Field(SecretID, "host") != "" && c.store.Field(SecretID, "username") != ""
}

// Fields lists the stored credential fields, for status reporting.
func (c *Client) Fields() []string { return c.store.FieldNames(SecretID) }

// Login stores IMAP credentials and verifies them by connecting once.
func (c *Client) Login(ctx context.Context, fields map[string]string) error {
	host := strings.TrimSpace(fields["host"])
	username := strings.TrimSpace(fields["username"])
	password := fields["password"]
	if host == "" || username == "" || password == "" {
		return errors.New("mailbox: host, username and password are required (for Gmail use an app password)")
	}
	update := map[string]string{
		"host":     host,
		"username": username,
		"password": password,
	}
	if port := strings.TrimSpace(fields["port"]); port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return fmt.Errorf("mailbox: port %q is not a number", port)
		}
		update["port"] = port
	}
	if folder := strings.TrimSpace(fields["mailbox"]); folder != "" {
		update["mailbox"] = folder
	}
	if err := c.store.Merge(SecretID, update); err != nil {
		return err
	}

	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Logout().Wait()
}

// Logout drops the stored mailbox credentials.
func (c *Client) Logout() error { return c.store.Delete(SecretID) }

// Search returns the messages from the given senders within [since, before].
func (c *Client) Search(ctx context.Context, senders []string, since, before time.Time, limit int) ([]Message, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if len(senders) == 0 {
		return nil, errors.New("mailbox: no sender addresses to search for")
	}

	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	folder := c.store.Field(SecretID, "mailbox")
	if folder == "" {
		folder = DefaultMailbox
	}
	if _, err := conn.Select(folder, nil).Wait(); err != nil {
		return nil, fmt.Errorf("mailbox: select %q: %w", folder, err)
	}

	seen := make(map[imap.UID]bool)
	var uids []imap.UID
	for _, sender := range senders {
		criteria := &imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: sender}},
		}
		if !since.IsZero() {
			criteria.Since = since
		}
		if !before.IsZero() {
			// IMAP BEFORE is exclusive on the date, so widen by a day.
			criteria.Before = before.AddDate(0, 0, 1)
		}
		data, err := conn.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return nil, fmt.Errorf("mailbox: search for %q: %w", sender, err)
		}
		for _, uid := range data.AllUIDs() {
			if !seen[uid] {
				seen[uid] = true
				uids = append(uids, uid)
			}
		}
	}
	if len(uids) == 0 {
		return nil, nil
	}

	// Newest first, then cap: mailboxes assign UIDs in arrival order.
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if limit > 0 && len(uids) > limit {
		uids = uids[:limit]
	}

	messages, err := conn.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		Envelope:    true,
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{}},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("mailbox: fetch: %w", err)
	}

	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		parsed, err := toMessage(msg)
		if err != nil {
			continue
		}
		out = append(out, parsed)
	}
	return out, nil
}

func (c *Client) connect(ctx context.Context) (*imapclient.Client, error) {
	host := c.store.Field(SecretID, "host")
	if host == "" {
		return nil, ErrNotConfigured
	}
	port := c.store.Field(SecretID, "port")
	if port == "" {
		port = "993"
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	type result struct {
		conn *imapclient.Client
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := imapclient.DialTLS(host+":"+port, nil)
		if err != nil {
			done <- result{nil, fmt.Errorf("mailbox: connect to %s:%s: %w", host, port, err)}
			return
		}
		if err := conn.Login(c.store.Field(SecretID, "username"), c.store.Field(SecretID, "password")).Wait(); err != nil {
			conn.Close()
			done <- result{nil, fmt.Errorf("mailbox: login as %s: %w", c.store.Field(SecretID, "username"), err)}
			return
		}
		done <- result{conn, nil}
	}()

	select {
	case <-dialCtx.Done():
		return nil, dialCtx.Err()
	case res := <-done:
		return res.conn, res.err
	}
}

func toMessage(buf *imapclient.FetchMessageBuffer) (Message, error) {
	body := buf.FindBodySection(&imap.FetchItemBodySection{})
	if body == nil {
		return Message{}, errors.New("no body section")
	}
	reader, err := mail.CreateReader(strings.NewReader(string(body)))
	if err != nil {
		return Message{}, err
	}
	defer reader.Close()

	msg := Message{UID: buf.UID}
	if buf.Envelope != nil {
		msg.Subject = buf.Envelope.Subject
		msg.Date = buf.Envelope.Date
		msg.ID = strings.Trim(buf.Envelope.MessageID, "<>")
		if len(buf.Envelope.From) > 0 {
			msg.From = buf.Envelope.From[0].Addr()
		}
	}
	if msg.Date.IsZero() {
		msg.Date = buf.InternalDate
	}

	var plain, html string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		header, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(part.Body, 4<<20))
		if err != nil {
			continue
		}
		contentType, _, _ := header.ContentType()
		switch {
		case strings.Contains(contentType, "text/plain") && plain == "":
			plain = string(content)
		case strings.Contains(contentType, "text/html") && html == "":
			html = string(content)
		}
	}

	switch {
	case plain != "":
		msg.Text = normalizeWhitespace(plain)
	case html != "":
		msg.Text = normalizeWhitespace(htmlToText(html))
	default:
		return Message{}, errors.New("no textual body")
	}
	if msg.ID == "" {
		msg.ID = strconv.FormatUint(uint64(buf.UID), 10)
	}
	return msg, nil
}
