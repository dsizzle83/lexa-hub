package tlsclient

import (
	"encoding/xml"
	"fmt"
)

// DeviceCapability is a parsed IEEE 2030.5 DeviceCapability resource.
// Only the fields needed for Milestone 2 are populated. As later
// milestones consume more of the resource tree, this struct grows.
//
// The XML namespace urn:ieee:std:2030.5:ns is the SEP 2.0 / IEEE
// 2030.5 default namespace and is mandatory for conformance. Our
// xml.Unmarshal call validates the XML structure but does not check
// the namespace — that's a separate concern (XSD validation) deferred
// to a future milestone.
type DeviceCapability struct {
	XMLName xml.Name `xml:"DeviceCapability"`
	Href    string   `xml:"href,attr"`

	EndDeviceListLink     *Link `xml:"EndDeviceListLink"`
	MirrorUsagePointLink  *Link `xml:"MirrorUsagePointListLink"`
	SelfDeviceLink        *Link `xml:"SelfDeviceLink"`
	TimeLink              *Link `xml:"TimeLink"`
}

// Link is a 2030.5 hyperlink to another resource. The All attribute
// (when present) is the count of items in the linked collection.
type Link struct {
	Href string `xml:"href,attr"`
	All  string `xml:"all,attr,omitempty"`
}

// FetchDCAP performs an HTTP GET on /dcap, parses the response, and
// returns the DeviceCapability struct. This is the canonical example
// of how higher-level resource fetches work in this client: each
// resource gets its own Fetch* method that wraps Get + parseHTTPResponse
// + xml.Unmarshal.
func (c *Client) FetchDCAP() (*DeviceCapability, error) {
	raw, err := c.Get("/dcap")
	if err != nil {
		return nil, fmt.Errorf("GET /dcap: %w", err)
	}

	resp, err := parseHTTPResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %d (want 200)", resp.StatusCode)
	}
	if resp.ContentType != "application/sep+xml" {
		return nil, fmt.Errorf("unexpected Content-Type %q (want application/sep+xml)", resp.ContentType)
	}

	var dcap DeviceCapability
	if err := xml.Unmarshal(resp.Body, &dcap); err != nil {
		return nil, fmt.Errorf("unmarshal DCAP XML: %w", err)
	}

	return &dcap, nil
}
