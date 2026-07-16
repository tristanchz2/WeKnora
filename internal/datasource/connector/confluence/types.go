// Package confluence implements the Atlassian Confluence 7.x data source connector for WeKnora.
//
// It syncs pages from Confluence spaces into WeKnora knowledge bases by exporting
// each page as a PDF via Confluence's built-in PDF export action.
//
// Confluence 7.x REST API docs:
//   - Auth:       Basic Auth (username + password)
//   - Spaces:     GET /rest/api/space
//   - Content:    GET /rest/api/content/{id}?expand=body.storage,version,space
//   - Children:   GET /rest/api/content/{id}/child/page
//   - Search:     GET /rest/api/content/search?cql=...
//   - PDF export: GET /spaces/flyingpdf/pdfpageexport.action?pageId={id}
//
// This connector targets Confluence Server / Data Center 7.x specifically.
// Cloud and 8.x+ may differ in API paths or PDF export mechanism.
package confluence

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

// Config holds Confluence-specific configuration for the data source connector.
// Uses Basic Auth (username + password) which is standard for Confluence 7.x Server.
type Config struct {
	// Base URL of the Confluence instance (e.g. "https://confluence.example.com")
	BaseURL string `json:"base_url"`

	// Username for Basic Auth
	Username string `json:"username"`

	// Password for Basic Auth
	Password string `json:"password"`
}

// parseConfluenceConfig extracts and validates Confluence config from DataSourceConfig.
func parseConfluenceConfig(config *types.DataSourceConfig) (*Config, error) {
	if config == nil {
		return nil, datasource.ErrInvalidConfig
	}

	creds := config.Credentials

	baseURL, _ := creds["base_url"].(string)
	if baseURL == "" {
		return nil, fmt.Errorf("%w: missing base_url", datasource.ErrInvalidCredentials)
	}

	username, _ := creds["username"].(string)
	if username == "" {
		return nil, fmt.Errorf("%w: missing username", datasource.ErrInvalidCredentials)
	}

	password, _ := creds["password"].(string)
	if password == "" {
		return nil, fmt.Errorf("%w: missing password", datasource.ErrInvalidCredentials)
	}

	return &Config{
		BaseURL:  baseURL,
		Username: username,
		Password: password,
	}, nil
}

// --- API response types ---

// confluenceSpace represents a Confluence space from GET /rest/api/space.
type confluenceSpace struct {
	ID          int    `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Homepage    struct {
		ID string `json:"id"`
	} `json:"homepage"`
	Description struct {
		View struct {
			Value string `json:"value"`
		} `json:"view"`
	} `json:"description"`
	Links struct {
		WebUI string `json:"webui"`
		Self  string `json:"self"`
	} `json:"_links"`
}

// confluenceSpaceListResponse is the response from GET /rest/api/space.
type confluenceSpaceListResponse struct {
	Results []confluenceSpace `json:"results"`
	Start   int               `json:"start"`
	Limit   int               `json:"limit"`
	Size    int               `json:"size"`
	Links   struct {
		Next string `json:"next"`
	} `json:"_links"`
}

// confluencePage represents a Confluence page/content object.
type confluencePage struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Space   struct {
		ID   int    `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"space"`
	Version struct {
		By struct {
			DisplayName string `json:"displayName"`
		} `json:"by"`
		When string `json:"when"`
	} `json:"version"`
	Ancestors []struct {
		ID string `json:"id"`
	} `json:"ancestors"`
	Links struct {
		WebUI string `json:"webui"`
		Self  string `json:"self"`
	} `json:"_links"`
}

// confluenceContentListResponse is the response from content listing APIs.
type confluenceContentListResponse struct {
	Results []confluencePage `json:"results"`
	Start   int              `json:"start"`
	Limit   int              `json:"limit"`
	Size    int              `json:"size"`
	Links   struct {
		Next string `json:"next"`
	} `json:"_links"`
}

// confluenceSearchResponse is the response from GET /rest/api/content/search.
type confluenceSearchResponse struct {
	Results []confluencePage `json:"results"`
	Start   int              `json:"start"`
	Limit   int              `json:"limit"`
	Size    int              `json:"size"`
	Links   struct {
		Next string `json:"next"`
	} `json:"_links"`
}

// confluenceCursor stores incremental sync state for Confluence.
// Follows the same per-resource pattern as Feishu's SpaceNodeTimes and Yuque's BookDocTimes.
type confluenceCursor struct {
	// LastSyncTime is the timestamp of the last successful sync.
	LastSyncTime time.Time `json:"last_sync_time"`

	// SpacePageTimes maps resource_id -> (page_id -> version.when).
	// Used to detect which pages have changed since last sync.
	SpacePageTimes map[string]map[string]string `json:"space_page_times,omitempty"`
}

// normalizeLastModified converts Confluence ISO 8601 time to a comparable string.
// Input: "2026-07-16T11:25:00.000+08:00" → Output: "2026-07-16 11:25"
func normalizeLastModified(isoStr string) string {
	if len(isoStr) < 16 {
		return isoStr
	}
	// Take first 16 chars "2026-07-16T11:25" and replace T with space
	result := isoStr[:16]
	result = result[:10] + " " + result[11:]
	return result
}

// parseConfluenceTimestamp attempts to parse a Confluence timestamp string.
func parseConfluenceTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	// Try RFC3339 first
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t
	}
	// Try the normalized format "2006-01-02 15:04"
	if t, err := time.Parse("2006-01-02 15:04", normalizeLastModified(ts)); err == nil {
		return t
	}
	return time.Time{}
}

// safeFilename sanitizes a string for use as a filename.
func safeFilename(name string) string {
	if name == "" {
		return "untitled"
	}
	// Remove or replace unsafe characters
	result := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 32 || c == 127 {
			continue
		}
		switch c {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			result = append(result, '_')
		default:
			result = append(result, c)
		}
	}
	s := string(result)
	// Truncate to reasonable length (UTF-8 safe)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// spaceToResource converts a Confluence space to a Resource for UI listing.
// HasChildren is false: spaces are directly selectable as a whole,
// no page-level expansion is needed — we sync the entire space.
func spaceToResource(space confluenceSpace, baseURL string) types.Resource {
	return types.Resource{
		ExternalID:  fmt.Sprintf("%d", space.ID),
		Name:        space.Name,
		Type:        "space",
		URL:         baseURL + space.Links.WebUI,
		Description: space.Description.View.Value,
		HasChildren: false,
		Metadata: map[string]interface{}{
			"space_key": space.Key,
			"space_id":  space.ID,
		},
	}
}

// marshalCursor converts a confluenceCursor to the generic map format for SyncCursor.
func marshalCursor(c *confluenceCursor) map[string]interface{} {
	if c == nil {
		return nil
	}
	b, _ := json.Marshal(c)
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	return m
}

// unmarshalCursor converts the generic SyncCursor.ConnectorCursor map to a confluenceCursor.
func unmarshalCursor(m map[string]interface{}) *confluenceCursor {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	var c confluenceCursor
	_ = json.Unmarshal(b, &c)
	return &c
}
