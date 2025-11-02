package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"ella.to/sse"

	"ella.to/pipe/signal"
)

type Server struct {
	mapper *Mapper
}

var _ http.Handler = (*Server)(nil)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	inbox := r.URL.Query().Get("inbox")
	if inbox == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		pusher, err := sse.NewHttpPusher(w, 30*time.Second)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		defer pusher.Close()

		mshCh := s.mapper.Get(inbox)
		var msgCount int64

		for {
			select {
			case <-r.Context().Done():
				return
			case msg, ok := <-mshCh:
				if !ok {
					return
				}

				b, err := json.Marshal(msg)
				if err != nil {
					continue
				}

				err = pusher.Push(&sse.Message{
					Id:    strconv.FormatInt(msgCount, 10),
					Event: "data",
					Data:  string(b),
				})
				if err != nil {
					return
				}
				msgCount++
			}
		}

	case http.MethodPost:
		msg := &signal.Msg{}
		if err := json.NewDecoder(r.Body).Decode(msg); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		select {
		case s.mapper.Get(inbox) <- msg:
			w.WriteHeader(http.StatusAccepted)
		default:
			http.Error(w, "Failed to send message", http.StatusInternalServerError)
			return
		}

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func NewServer() *Server {
	return &Server{
		mapper: NewMapper(),
	}
}

type Client struct {
	host       string
	httpClient *http.Client
}

var _ signal.Signal = (*Client)(nil)

func (c *Client) Send(ctx context.Context, inbox string, msg *signal.Msg) error {
	url, err := c.getURL(inbox)
	if err != nil {
		return err
	}

	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		return nil
	}

	errMsg, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	return fmt.Errorf("failed to send message, status code: %d: %s", resp.StatusCode, errMsg)
}

func (c *Client) Receiver(inbox string) (signal.Receiver, error) {
	url, err := c.getURL(inbox)
	if err != nil {
		return nil, err
	}

	receiver, err := sse.NewHttpReceiver(url)
	if err != nil {
		return nil, err
	}

	return signal.ReceiverFunc(func(ctx context.Context) (*signal.Msg, error) {
		msg, err := receiver.Receive(ctx)
		if err != nil {
			return nil, err
		}

		sigMsg := &signal.Msg{}
		if err := json.Unmarshal([]byte(msg.Data), sigMsg); err != nil {
			return nil, err
		}

		return sigMsg, nil
	}), nil
}

func (c *Client) getURL(inbox string) (string, error) {
	u, err := url.Parse(c.host)
	if err != nil {
		return "", err
	}

	q := u.Query()
	q.Set("inbox", inbox)
	u.RawQuery = q.Encode()

	return u.String(), nil
}

func NewClient(host string) (*Client, error) {
	client := &Client{
		host:       host,
		httpClient: &http.Client{},
	}

	return client, nil
}
