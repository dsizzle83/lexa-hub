package tlsclient

import "encoding/xml"

func xmlUnmarshalImpl(data []byte, v interface{}) error {
	return xml.Unmarshal(data, v)
}
