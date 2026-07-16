package confluence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/logger"
)

// Client wraps the Confluence 7.x REST API with Basic Auth.
type Client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

// NewClient creates a new Confluence API client.
func NewClient(config *Config) (*Client, error) {
	baseURL := strings.TrimRight(config.BaseURL, "/")
	if err := datasource.ValidateConnectorBaseURL(baseURL); err != nil {
		return nil, err
	}
	return &Client{
		baseURL:    baseURL,
		username:   config.Username,
		password:   config.Password,
		httpClient: datasource.NewConnectorHTTPClient(60 * time.Second),
	}, nil
}

// Ping tests connectivity by fetching the current user or a lightweight endpoint.
func (c *Client) Ping(ctx context.Context) error {
	// Use /rest/api/space with limit=1 as a lightweight connectivity check
	_, err := c.doRequest(ctx, http.MethodGet, "/rest/api/space?limit=1", nil)
	if err != nil {
		return fmt.Errorf("ping %s failed (user=%s): %w", c.baseURL, c.username, err)
	}
	return nil
}

// doRequest performs an authenticated HTTP request to the Confluence API.
func (c *Client) doRequest(ctx context.Context, method, path string, result interface{}) ([]byte, error) {
	fullURL := c.baseURL + path
	if !strings.HasPrefix(path, "http") {
		fullURL = c.baseURL + path
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	logger.Infof(ctx, "[Confluence] %s %s", method, path)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP %s %s failed: %w", method, fullURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	logger.Infof(ctx, "[Confluence] %s %s → status=%d bodyLen=%d",
		method, path, resp.StatusCode, len(body))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("confluence API error: status=%d body=%s",
			resp.StatusCode, truncate(string(body), 500))
	}

	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
	}

	return body, nil
}

// doRequestRaw performs an authenticated HTTP request and returns raw bytes (for PDF download).
func (c *Client) doRequestRaw(ctx context.Context, path string) ([]byte, string, error) {
	fullURL := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/pdf, application/octet-stream, */*")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("HTTP GET %s failed: %w", fullURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("confluence API error: status=%d body=%s",
			resp.StatusCode, truncate(string(body), 500))
	}

	contentType := resp.Header.Get("Content-Type")
	return body, contentType, nil
}

// --- Space APIs ---

// ListSpaces returns all accessible Confluence spaces.
func (c *Client) ListSpaces(ctx context.Context) ([]confluenceSpace, error) {
	var allSpaces []confluenceSpace
	start := 0
	limit := 50

	for {
		path := fmt.Sprintf("/rest/api/space?start=%d&limit=%d", start, limit)
		var resp confluenceSpaceListResponse
		_, err := c.doRequest(ctx, http.MethodGet, path, &resp)
		if err != nil {
			return nil, fmt.Errorf("list spaces (start=%d): %w", start, err)
		}

		allSpaces = append(allSpaces, resp.Results...)

		if resp.Links.Next == "" || len(resp.Results) < limit {
			break
		}
		start += len(resp.Results)
	}

	return allSpaces, nil
}

// --- Content APIs ---

// GetPage retrieves a single page by ID with version and space info.
func (c *Client) GetPage(ctx context.Context, pageID string) (*confluencePage, error) {
	path := fmt.Sprintf("/rest/api/content/%s?expand=version,space", pageID)
	var page confluencePage
	_, err := c.doRequest(ctx, http.MethodGet, path, &page)
	if err != nil {
		return nil, err
	}
	return &page, nil
}

// GetAllPagesInSpace returns ALL current pages in a space using CQL,
// without requiring recursive parent→child traversal.
// This mirrors the working Python approach: type=page AND space=xxx.
func (c *Client) GetAllPagesInSpace(ctx context.Context, spaceKey string) ([]confluencePage, error) {
	var allPages []confluencePage
	start := 0
	limit := 100

	cql := fmt.Sprintf(`type=page AND space="%s" order by lastModified desc`, spaceKey)

	for {
		path := fmt.Sprintf("/rest/api/content/search?cql=%s&start=%d&limit=%d&expand=version,space",
			url.QueryEscape(cql), start, limit)
		var resp confluenceSearchResponse
		_, err := c.doRequest(ctx, http.MethodGet, path, &resp)
		if err != nil {
			return nil, fmt.Errorf("search all pages in space %s (start=%d): %w", spaceKey, start, err)
		}

		allPages = append(allPages, resp.Results...)

		if resp.Links.Next == "" || len(resp.Results) < limit {
			break
		}
		start += len(resp.Results)
	}

	return allPages, nil
}

// GetTrashedPagesBySpace returns all pages in the trash for a given space.
func (c *Client) GetTrashedPagesBySpace(ctx context.Context, spaceKey string) ([]confluencePage, error) {
	var allPages []confluencePage
	start := 0
	limit := 100

	for {
		path := fmt.Sprintf("/rest/api/content?spaceKey=%s&status=trashed&type=page&start=%d&limit=%d",
			url.QueryEscape(spaceKey), start, limit)
		var resp confluenceContentListResponse
		_, err := c.doRequest(ctx, http.MethodGet, path, &resp)
		if err != nil {
			// If the space doesn't exist or we get an error, return what we have
			logger.Warnf(ctx, "[Confluence] failed to list trashed pages for space %s: %v", spaceKey, err)
			break
		}

		allPages = append(allPages, resp.Results...)

		if resp.Links.Next == "" || len(resp.Results) < limit {
			break
		}
		start += len(resp.Results)
	}

	return allPages, nil
}

// --- PDF Export ---

// ExportPageAsPDF exports a Confluence page as PDF using the built-in export action.
// Returns the PDF bytes and a suggested filename.
func (c *Client) ExportPageAsPDF(ctx context.Context, pageID string, pageTitle string) ([]byte, string, error) {
	path := fmt.Sprintf("/spaces/flyingpdf/pdfpageexport.action?pageId=%s", pageID)

	data, contentType, err := c.doRequestRaw(ctx, path)
	if err != nil {
		return nil, "", fmt.Errorf("export page %s as PDF: %w", pageID, err)
	}

	// Validate that we actually got a PDF
	if !strings.Contains(strings.ToLower(contentType), "pdf") && len(data) < 100 {
		return nil, "", fmt.Errorf("export page %s: response is not a PDF (content-type: %s, size: %d)",
			pageID, contentType, len(data))
	}

	// Build filename
	filename := safeFilename(pageTitle) + ".pdf"

	logger.Infof(ctx, "[Confluence] exported page %s (%s) as PDF: %d bytes",
		pageID, pageTitle, len(data))

	return data, filename, nil
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
