package web

import _ "embed"

//go:embed assets/htmx.min.js
var HtmxJS []byte

//go:embed assets/favicon.svg
var FaviconSVG []byte
