package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const MIN_POLLING_DELAY = 1
const NOTIFICATION_BUS = "org.freedesktop.Notifications"
const NOTIFICATION_PATH = "/org/freedesktop/Notifications"
const NOTIFICATION_IFACE = "org.freedesktop.Notifications"

type NotificationResult int

var token string
var user_id string
var client = &http.Client{}

func getToken() error {
	token_bytes, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return err
	}

	token = strings.TrimSpace(string(token_bytes))
	return nil
}

type Notification struct {
	Id      string `json:"id"`
	Unread  bool   `json:"unread"`
	Reason  string `json:"reason"`
	Subject struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"subject"`
	Repository struct {
		Id int `json:"id"`
	} `json:"repository"`
}

type Pull struct {
	HtmlURL  string `json:"html_url"`
	IssueURL string `json:"issue_url"`
}

func fetch(req *http.Request) ([]Notification, int, error) {

	res, err := client.Do(req)
	if err != nil {
		return nil, MIN_POLLING_DELAY, err
	}

	poll_interval := 0
	poll_interval_header := res.Header.Get("X-Poll-Interval")
	if poll_interval_header != "" {
		poll_interval, err = strconv.Atoi(poll_interval_header)
		if err != nil {
			fmt.Printf("error parsing poll interval: %s", err.Error())
			poll_interval = 0
		}
	}

	if res.StatusCode == http.StatusOK {

		// Only reset modified since when status OK
		modified_since_header := res.Header.Get("Last-Modified")
		if modified_since_header != "" {
			// Set for the next request
			req.Header.Set("If-Modified-Since", modified_since_header)
		}

		decoder := json.NewDecoder(res.Body)
		var notification_array []Notification
		if err := decoder.Decode(&notification_array); err != nil {
			return nil, MIN_POLLING_DELAY, err
		}

		res := []Notification{}
		for _, notification := range notification_array {
			if !notification.Unread || !(notification.Reason == "approval_requested" ||
				notification.Reason == "assign" ||
				notification.Reason == "author" ||
				notification.Reason == "ci_activity" ||
				notification.Reason == "comment" ||
				notification.Reason == "mention" ||
				notification.Reason == "review_requested" ||
				notification.Reason == "team_mention") {
				continue
			}

			res = append(res, notification)
		}

		return res, max(MIN_POLLING_DELAY, poll_interval), nil
	}

	if res.StatusCode == http.StatusNotModified {
		// No new notifications
		return []Notification{}, MIN_POLLING_DELAY, nil
	}

	return []Notification{}, max(MIN_POLLING_DELAY, poll_interval), nil
}

func fetchIssueID(url string) (int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, fmt.Errorf("error building request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Accept", "application/vnd.github+json")

	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}

	decoder := json.NewDecoder(res.Body)
	var issue struct {
		Id int `json:"id"`
	}
	if err := decoder.Decode(&issue); err != nil {
		return 0, err
	}

	return issue.Id, nil
}

func fetchURL(url string, repo int) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("error building request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Accept", "application/vnd.github+json")

	res, err := client.Do(req)
	if err != nil {
		return "", err
	}

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("error fetching html url: %s", res.Status)
	}

	decoder := json.NewDecoder(res.Body)
	var pull Pull
	if err := decoder.Decode(&pull); err != nil {
		return "", err
	}

	issue, err := fetchIssueID(pull.IssueURL)
	if err != nil {
		return "", err
	}

	payload := []byte(fmt.Sprintf("Repository;%d;Issue;%d", repo, issue))

	notification_referrer_id := strings.Trim("NT_"+base64.URLEncoding.EncodeToString(
		append(
			[]byte{147, 1, 206, 4, 110, 16, 168, 218, 0, byte(len(payload))},
			payload...,
		)), "=")

	return fmt.Sprintf("%s?notification_referrer_id=%s", pull.HtmlURL, notification_referrer_id), nil
}

func markAsRead(id string) error {
	req, err := http.NewRequest("PATCH", fmt.Sprintf("https://api.github.com/notifications/threads/%s", id), nil)
	if err != nil {
		panic(fmt.Errorf("error building request: %w", err))
	}

	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Accept", "application/vnd.github+json")

	res, err := client.Do(req)
	if err != nil {
		return err
	}

	if res.StatusCode != http.StatusResetContent {
		return fmt.Errorf("error marking as read: %s", res.Status)
	}

	return nil
}

type NotificationInstance struct {
	url  string
	repo int
	id   string
}

var notification_instances = make(map[uint32]NotificationInstance)

func notify(conn *dbus.Conn, title string, body string, url string, thread_id string, repo int, timeout time.Duration) error {
	actions := []string{
		"default",
		"Open",
	}

	hints := map[string]dbus.Variant{}

	var id uint32

	err := conn.Object(NOTIFICATION_BUS, NOTIFICATION_PATH).Call(
		NOTIFICATION_IFACE+".Notify",
		0,
		"gh-notify",
		uint32(0), // ?
		"",        // Icon
		title,
		body,
		actions,
		hints,
		int32(timeout.Milliseconds()),
	).Store(&id)

	if err != nil {
		return err
	}

	notification_instances[id] = NotificationInstance{url, repo, thread_id}

	return nil
}

func main() {
	conn, err := dbus.SessionBus()
	if err != nil {
		panic(fmt.Errorf("bus error: %w", err))
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(NOTIFICATION_IFACE),
		dbus.WithMatchMember("ActionInvoked"),
	); err != nil {
		panic(err)
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(NOTIFICATION_IFACE),
		dbus.WithMatchMember("NotificationClosed"),
	); err != nil {
		panic(err)
	}

	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)

	go func() {
		for {
			sig := <-signals
			switch sig.Name {
			case NOTIFICATION_IFACE + ".ActionInvoked":
				if len(sig.Body) != 2 {
					continue
				}

				id, ok := sig.Body[0].(uint32)
				if !ok {
					continue
				}

				if instance, ok := notification_instances[id]; ok {
					action, ok := sig.Body[1].(string)
					if !ok {
						continue
					}

					if action == "default" {
						// Spawn go-routines to open URL and mark as read
						go func() {
							url, err := fetchURL(instance.url, instance.repo)
							if err != nil {
								fmt.Printf(`error fetching html url "%s": %s\n`, url, err.Error())
							}

							if err := exec.Command("xdg-open", url).Start(); err != nil {
								fmt.Printf(`error opening link "%s": %s\n`, url, err.Error())
							}
						}()

						go markAsRead(instance.id)

						delete(notification_instances, id)
					}
				}
			case NOTIFICATION_IFACE + ".NotificationClosed":
				if len(sig.Body) != 2 {
					continue
				}

				id, ok := sig.Body[0].(uint32)
				if !ok {
					continue
				}

				if _, ok := notification_instances[id]; ok {
					delete(notification_instances, id)
				}
			}
		}
	}()

	if err := getToken(); err != nil {
		panic(fmt.Errorf("token error: %w", err))
	}

	req, err := http.NewRequest("GET", "https://api.github.com/notifications", nil)
	if err != nil {
		panic(fmt.Errorf("error building request: %w", err))
	}

	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Accept", "application/vnd.github+json")

	for {
		notifications, delay, err := fetch(req)
		if err != nil {
			fmt.Fprint(os.Stderr, "%s\n", err.Error())
		}

		for _, notification := range notifications {
			if err = notify(conn, notification.Subject.Title, notification.Reason, notification.Subject.URL, notification.Id, notification.Repository.Id, time.Duration(15)*time.Second); err != nil {
				fmt.Fprint(os.Stderr, "%s\n", err.Error())
			}
		}

		// Wait before re-polling
		time.Sleep(time.Duration(delay) * time.Second)
	}
}
