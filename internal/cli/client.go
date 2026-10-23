package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// Client talks to a running node's admin API.
type Client struct {
	Base  string
	Token string
	http  *http.Client
}

func NewClient(base, token string) *Client {
	return &Client{Base: base, Token: token, http: &http.Client{Timeout: 10 * time.Minute}}
}

// Do performs a request and decodes a JSON response into out.
func (c *Client) Do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("%s", e.Error.Message)
		}
		return fmt.Errorf("node returned HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Stream performs a request without a timeout (uploads and downloads of any
// size) and returns the response for the caller to read and close; HTTP
// errors are decoded into Go errors.
func (c *Client) Stream(method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("%s", e.Error.Message)
		}
		return nil, fmt.Errorf("node returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// Upload streams a multipart form with one file to path.
func (c *Client) Upload(path, filename string, fields map[string]string, r io.Reader, out any) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var err error
		for k, v := range fields {
			if err = mw.WriteField(k, v); err != nil {
				break
			}
		}
		if err == nil {
			var part io.Writer
			if part, err = mw.CreateFormFile("file", filename); err == nil {
				if _, err = io.Copy(part, r); err == nil {
					err = mw.Close()
				}
			}
		}
		pw.CloseWithError(err)
	}()
	resp, err := c.Stream("POST", path, mw.FormDataContentType(), pr)
	pr.Close()
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}
