package confluence

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/Tencent/WeKnora/internal/logger"
)

// ─────────────────────────────────────────────────────────────────────────────
// Main entry: fetch export_view HTML, inline images, render to PDF.
// ─────────────────────────────────────────────────────────────────────────────

// exportViewPageToPDF fetches a Confluence Cloud page's body.export_view HTML,
// inlines all embedded images as base64 data URIs, wraps the result in a
// styled HTML document, and renders it to PDF via headless Chrome.
//
// Returns the PDF bytes and a suggested filename.
func exportViewPageToPDF(ctx context.Context, client *Client, pageID, pageTitle string) ([]byte, string, error) {
	// ── Step 1: Fetch page with body.export_view ──────────────────────
	logger.Infof(ctx, "[Confluence] fetching export_view for page %s", pageID)

	exportHTML, err := fetchExportViewHTML(ctx, client, pageID)
	if err != nil {
		return nil, "", err
	}

	// ── Step 2: Fetch attachment list ─────────────────────────────────
	attachments, err := fetchAttachmentList(ctx, client, pageID)
	if err != nil {
		logger.Warnf(ctx, "[Confluence] failed to list attachments for page %s: %v", pageID, err)
		// Non-fatal: images may still render via src URLs.
	}

	attMap := make(map[string]string, len(attachments))
	for _, a := range attachments {
		attMap[a.Title] = a.ID
	}
	logger.Infof(ctx, "[Confluence] page %s: %d attachments", pageID, len(attachments))

	// ── Step 3: Clean Confluence storage-format junk & inline images ──
	exportHTML = cleanConfluenceHTML(exportHTML)
	exportHTML = inlineImages(ctx, client, pageID, attMap, exportHTML)

	// ── Step 4: Wrap in full HTML document ────────────────────────────
	fullHTML := wrapHTMLDocument(pageTitle, exportHTML)

	// ── Step 5: Render to PDF via chromedp ────────────────────────────
	logger.Infof(ctx, "[Confluence] rendering PDF for page %s (%s)", pageID, pageTitle)
	pdfData, err := renderToPDF(ctx, fullHTML)
	if err != nil {
		return nil, "", fmt.Errorf("render PDF for page %s: %w", pageID, err)
	}

	filename := safeFilename(pageTitle) + ".pdf"
	logger.Infof(ctx, "[Confluence] exported page %s (%s) as PDF: %d bytes",
		pageID, pageTitle, len(pdfData))

	return pdfData, filename, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// API helpers
// ─────────────────────────────────────────────────────────────────────────────

// fetchExportViewHTML retrieves the body.export_view HTML for a page.
func fetchExportViewHTML(ctx context.Context, client *Client, pageID string) (string, error) {
	path := fmt.Sprintf("/rest/api/content/%s?expand=body.export_view,space", pageID)

	var pageResp confluencePageWithExportView
	_, err := client.doRequest(ctx, http.MethodGet, path, &pageResp)
	if err != nil {
		return "", fmt.Errorf("fetch page %s export_view: %w", pageID, err)
	}

	html := pageResp.Body.ExportView.Value
	if html == "" {
		return "", fmt.Errorf("page %s: empty body.export_view", pageID)
	}
	return html, nil
}

// fetchAttachmentList retrieves all attachments for a page, handling pagination.
func fetchAttachmentList(ctx context.Context, client *Client, pageID string) ([]confluenceAttachment, error) {
	var allAttachments []confluenceAttachment
	start := 0
	limit := 50

	for {
		path := fmt.Sprintf("/rest/api/content/%s/child/attachment?start=%d&limit=%d",
			pageID, start, limit)
		var resp confluenceAttachmentResponse
		_, err := client.doRequest(ctx, http.MethodGet, path, &resp)
		if err != nil {
			return nil, fmt.Errorf("fetch attachments for page %s (start=%d): %w", pageID, start, err)
		}

		allAttachments = append(allAttachments, resp.Results...)

		if len(resp.Results) < limit {
			break
		}
		start += len(resp.Results)
	}

	return allAttachments, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// HTML cleaning
// ─────────────────────────────────────────────────────────────────────────────

// Pre-compiled regexes for HTML cleaning.
var (
	reAcLayout             = regexp.MustCompile(`(?i)<ac:layout[^>]*>`)
	reAcLayoutClose        = regexp.MustCompile(`(?i)</ac:layout>`)
	reAcLayoutSection      = regexp.MustCompile(`(?i)<ac:layout-section[^>]*>`)
	reAcLayoutSectionClose = regexp.MustCompile(`(?i)</ac:layout-section>`)
	reAcLayoutCellWidth    = regexp.MustCompile(`(?i)<ac:layout-cell\s+ac:width="(\d+)"[^>]*>`)
	reAcLayoutCell         = regexp.MustCompile(`(?i)<ac:layout-cell[^>]*>`)
	reAcLayoutCellClose    = regexp.MustCompile(`(?i)</ac:layout-cell>`)
	reAcLayoutCellContent  = regexp.MustCompile(`(?i)<ac:layout-cell-content[^>]*>`)
	reAcLayoutCellContentC = regexp.MustCompile(`(?i)</ac:layout-cell-content>`)
	reAcImageSelfClose     = regexp.MustCompile(`(?i)<ac:image[^>]*/>`);
	reAcImageOpen          = regexp.MustCompile(`(?i)<ac:image[^>]*>`)
	reAcImageClose         = regexp.MustCompile(`(?i)</ac:image>`)
	reRiAttachmentSelf     = regexp.MustCompile(`(?i)<ri:attachment[^>]*/>`);
	reRiAttachmentPair     = regexp.MustCompile(`(?i)<ri:attachment[^>]*>.*?</ri:attachment>`)
	reRiPageSelfClose      = regexp.MustCompile(`(?i)<ri:page[^>]*/>`);
	reImgAlign             = regexp.MustCompile(`(?i)(<img\s[^>]*?)\bac:align="([^"]*)"([^>]*>)`)
	reStyleAttr            = regexp.MustCompile(`(?i)\bstyle="([^"]*)"`)
	reAcAlignAttr          = regexp.MustCompile(`(?i)\sac:align="[^"]*"`)
	reAcLayoutFitAttr      = regexp.MustCompile(`(?i)\sac:layout-fit-to-page="[^"]*"`)
	reAcHeightAttr         = regexp.MustCompile(`(?i)\sac:height="[^"]*"`)
	reAcWidthAttr          = regexp.MustCompile(`(?i)\sac:width="[^"]*"`)
	reDataMceSrc           = regexp.MustCompile(`(?i)\sdata-mce-src="[^"]*"`)

	// Image inlining regexes.
	reImgTag     = regexp.MustCompile(`(?i)<img\s[^>]*>`)
	reSrcAttr    = regexp.MustCompile(`(?i)\bsrc\s*=\s*"([^"]*)"`)
	reAltAttr    = regexp.MustCompile(`(?i)\balt\s*=\s*"([^"]*)"`)
	reResourceID = regexp.MustCompile(`(?i)\bdata-linked-resource-id\s*=\s*"([^"]*)"`)
)

// cleanConfluenceHTML converts Confluence storage-format elements into
// valid HTML so the browser PDF renderer can display them correctly.
//
// Mapping:
//
//	ac:layout              → <table>
//	ac:layout-section      → <tr>
//	ac:layout-cell         → <td>  (ac:width → style width)
//	ac:layout-cell-content → <div>
//	ac:image               → unwrapped (children kept)
//	ri:attachment / ri:page→ removed (metadata only)
//	ac:align on <img>      → inline CSS alignment
//	data-mce-src           → removed (stale duplicate of src)
func cleanConfluenceHTML(html string) string {
	// ── Layout → table ────────────────────────────────────────────
	html = reAcLayout.ReplaceAllString(html, `<table style="width:100%;border-collapse:collapse;border:none;">`)
	html = reAcLayoutClose.ReplaceAllString(html, `</table>`)

	html = reAcLayoutSection.ReplaceAllString(html, `<tr>`)
	html = reAcLayoutSectionClose.ReplaceAllString(html, `</tr>`)

	html = reAcLayoutCellWidth.ReplaceAllString(html, `<td style="vertical-align:top;width:${1}px;border:none;padding:4px;">`)
	html = reAcLayoutCell.ReplaceAllString(html, `<td style="vertical-align:top;border:none;padding:4px;">`)
	html = reAcLayoutCellClose.ReplaceAllString(html, `</td>`)

	html = reAcLayoutCellContent.ReplaceAllString(html, `<div>`)
	html = reAcLayoutCellContentC.ReplaceAllString(html, `</div>`)

	// ── ac:image → unwrap (keep children) ─────────────────────────
	html = reAcImageSelfClose.ReplaceAllString(html, "")
	html = reAcImageOpen.ReplaceAllString(html, "")
	html = reAcImageClose.ReplaceAllString(html, "")

	// ── ri:attachment / ri:page → remove (metadata) ───────────────
	html = reRiAttachmentSelf.ReplaceAllString(html, "")
	html = reRiAttachmentPair.ReplaceAllString(html, "")
	html = reRiPageSelfClose.ReplaceAllString(html, "")

	// ── <img> ac:align → CSS alignment ────────────────────────────
	html = reImgAlign.ReplaceAllStringFunc(html, func(tag string) string {
		m := reImgAlign.FindStringSubmatch(tag)
		if m == nil {
			return tag
		}
		prefix, align, suffix := m[1], strings.ToLower(m[2]), m[3]
		var css string
		switch align {
		case "center":
			css = "display:block;margin-left:auto;margin-right:auto;"
		case "right":
			css = "float:right;"
		case "left":
			css = "float:left;"
		}
		if css != "" {
			if sm := reStyleAttr.FindStringSubmatch(prefix + suffix); sm != nil {
				old := sm[0]
				newAttr := fmt.Sprintf(`style="%s%s"`, sm[1], css)
				tag = strings.Replace(tag, old, newAttr, 1)
			} else {
				tag = fmt.Sprintf(`%s style="%s"%s`, prefix, css, suffix)
			}
		}
		tag = reAcAlignAttr.ReplaceAllString(tag, "")
		return tag
	})

	// Remove other ac:* attributes from tags.
	html = reAcLayoutFitAttr.ReplaceAllString(html, "")
	html = reAcHeightAttr.ReplaceAllString(html, "")
	html = reAcWidthAttr.ReplaceAllString(html, "")
	html = reDataMceSrc.ReplaceAllString(html, "")

	return html
}

// ─────────────────────────────────────────────────────────────────────────────
// Image inlining
// ─────────────────────────────────────────────────────────────────────────────

// inlineImages finds all <img> tags and replaces each src with a base64
// data URI by downloading via the attachment download API.
//
// Attachment ID resolution order:
//  1. data-linked-resource-id attribute (most reliable, direct ID)
//  2. filename from src URL → matched against attachment list
func inlineImages(ctx context.Context, client *Client, pageID string,
	attMap map[string]string, html string) string {

	return reImgTag.ReplaceAllStringFunc(html, func(imgTag string) string {
		srcMatch := reSrcAttr.FindStringSubmatch(imgTag)
		if srcMatch == nil {
			return imgTag
		}
		src := srcMatch[1]

		// Skip already-inlined data URIs.
		if strings.HasPrefix(src, "data:") {
			return imgTag
		}

		// ── Method 1: data-linked-resource-id ─────────────────────
		var attID string
		if m := reResourceID.FindStringSubmatch(imgTag); m != nil {
			attID = m[1]
		}

		// ── Method 2: filename lookup ─────────────────────────────
		var filename string
		if attID == "" {
			filename = extractFilenameFromURL(src)
			if filename == "" {
				filename = extractAltFromTag(imgTag)
			}
			if filename != "" {
				attID = attMap[filename]
			}
		}

		if attID == "" {
			logger.Warnf(ctx, "[Confluence] cannot resolve attachment for src=%q, skipping",
				truncate(src, 80))
			return imgTag
		}

		// Download via attachment API.
		downloadPath := fmt.Sprintf("/rest/api/content/%s/child/attachment/%s/download?os_authType=basic",
			pageID, attID)

		label := filename
		if label == "" {
			label = "attachment-" + attID
		}
		logger.Infof(ctx, "[Confluence] downloading %s (att=%s)", label, attID)

		imgBytes, contentType, err := client.doRequestRaw(ctx, downloadPath)
		if err != nil {
			logger.Warnf(ctx, "[Confluence] download failed for %s: %v", label, err)
			return imgTag
		}

		if contentType == "" {
			contentType = "image/png"
		}
		contentType = strings.Split(contentType, ";")[0]
		contentType = strings.TrimSpace(contentType)

		b64 := base64.StdEncoding.EncodeToString(imgBytes)
		dataURI := fmt.Sprintf("data:%s;base64,%s", contentType, b64)

		newTag := strings.Replace(imgTag, src, dataURI, 1)
		newTag = reDataMceSrc.ReplaceAllString(newTag, "")
		logger.Infof(ctx, "[Confluence] inlined %s: %d bytes -> %s", label, len(imgBytes), contentType)
		return newTag
	})
}

// extractFilenameFromURL extracts the filename from a URL like
// "https://xxx/wiki/download/attachments/123/image.png?api=v2"
func extractFilenameFromURL(rawURL string) string {
	if idx := strings.Index(rawURL, "?"); idx >= 0 {
		rawURL = rawURL[:idx]
	}
	if idx := strings.LastIndex(rawURL, "/"); idx >= 0 {
		return rawURL[idx+1:]
	}
	return rawURL
}

// extractAltFromTag extracts the alt attribute value from an <img> tag.
func extractAltFromTag(imgTag string) string {
	m := reAltAttr.FindStringSubmatch(imgTag)
	if m != nil {
		return m[1]
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────────
// HTML document wrapper
// ─────────────────────────────────────────────────────────────────────────────

// wrapHTMLDocument wraps an HTML body fragment in a full document with CSS.
func wrapHTMLDocument(title, bodyHTML string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>%s</title>
<style>
  @page { size: A4; margin: 20mm; }
  html { font-size: 14px; }
  body { font-family: -apple-system, "Segoe UI", Helvetica, Arial, sans-serif; line-height: 1.6; color: #333; }
  h1 { font-size: 1.8em; margin-bottom: 0.5em; border-bottom: 1px solid #ccc; padding-bottom: 0.3em; }
  h2 { font-size: 1.4em; }
  h3 { font-size: 1.2em; }
  img { max-width: 100%% !important; height: auto !important; width: auto !important; }
  table { border-collapse: collapse; width: 100%%; }
  td, th { border: 1px solid #ccc; padding: 6px 10px; text-align: left; }
  th { background: #f5f5f5; font-weight: 600; }
  pre, code { background: #f5f5f5; padding: 2px 4px; border-radius: 3px; font-size: 0.9em; }
  pre { padding: 12px; overflow-x: auto; }
  blockquote { border-left: 3px solid #ccc; margin-left: 0; padding-left: 1em; color: #555; }
</style>
</head>
<body>
<h1>%s</h1>
%s
</body>
</html>`, title, title, bodyHTML)
}

// ─────────────────────────────────────────────────────────────────────────────
// PDF rendering via chromedp
// ─────────────────────────────────────────────────────────────────────────────

// renderToPDF renders an HTML string to PDF using headless Chrome.
func renderToPDF(ctx context.Context, htmlContent string) ([]byte, error) {
	tmpFile, err := os.CreateTemp("", "confluence-*.html")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(htmlContent); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("write temp HTML: %w", err)
	}
	tmpFile.Close()

	fileURL := "file://" + tmpFile.Name()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(func(string, ...interface{}) {}))
	defer taskCancel()

	if err := chromedp.Run(taskCtx,
		chromedp.Navigate(fileURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(2*time.Second),
	); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}

	var pdfBuf []byte
	if err := chromedp.Run(taskCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			pdfBuf, _, err = page.PrintToPDF().
				WithPaperWidth(8.27).
				WithPaperHeight(11.69).
				WithPrintBackground(true).
				Do(ctx)
			return err
		}),
	); err != nil {
		return nil, fmt.Errorf("print to PDF: %w", err)
	}

	return pdfBuf, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

