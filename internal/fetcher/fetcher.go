package fetcher

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"yggpeers/internal/store"
)

const (
	repoArchiveURL      = "https://github.com/yggdrasil-network/public-peers/archive/refs/heads/master.tar.gz"
	maxArchiveBytes     = 10 << 20
	maxExpandedBytes    = 50 << 20
	maxPeerFileBytes    = 1 << 20
	archiveFetchTimeout = 30 * time.Second
)

var peerURIRe = regexp.MustCompile("`((?:tcp|tls|quic|ws|wss|socks|sockstls)://[^`\\r\\n]+)`")

var isoToCountry = map[string]string{
	"AD": "Andorra",
	"AE": "United Arab Emirates",
	"AF": "Afghanistan",
	"AG": "Antigua and Barbuda",
	"AL": "Albania",
	"AM": "Armenia",
	"AO": "Angola",
	"AR": "Argentina",
	"AT": "Austria",
	"AU": "Australia",
	"AZ": "Azerbaijan",
	"BA": "Bosnia and Herzegovina",
	"BB": "Barbados",
	"BD": "Bangladesh",
	"BE": "Belgium",
	"BF": "Burkina Faso",
	"BG": "Bulgaria",
	"BH": "Bahrain",
	"BI": "Burundi",
	"BJ": "Benin",
	"BN": "Brunei",
	"BO": "Bolivia",
	"BR": "Brazil",
	"BS": "Bahamas",
	"BT": "Bhutan",
	"BW": "Botswana",
	"BY": "Belarus",
	"BZ": "Belize",
	"CA": "Canada",
	"CD": "Congo (Democratic Republic of the)",
	"CF": "Central African Republic",
	"CG": "Congo",
	"CH": "Switzerland",
	"CI": "Côte d'Ivoire",
	"CL": "Chile",
	"CM": "Cameroon",
	"CN": "China",
	"CO": "Colombia",
	"CR": "Costa Rica",
	"CU": "Cuba",
	"CY": "Cyprus",
	"CZ": "Czech Republic",
	"DE": "Germany",
	"DJ": "Djibouti",
	"DK": "Denmark",
	"DO": "Dominican Republic",
	"DZ": "Algeria",
	"EC": "Ecuador",
	"EE": "Estonia",
	"EG": "Egypt",
	"ER": "Eritrea",
	"ES": "Spain",
	"ET": "Ethiopia",
	"FI": "Finland",
	"FJ": "Fiji",
	"FM": "Micronesia",
	"FR": "France",
	"GA": "Gabon",
	"GB": "United Kingdom",
	"GD": "Grenada",
	"GE": "Georgia",
	"GH": "Ghana",
	"GM": "Gambia",
	"GN": "Guinea",
	"GQ": "Equatorial Guinea",
	"GR": "Greece",
	"GT": "Guatemala",
	"GW": "Guinea-Bissau",
	"GY": "Guyana",
	"HN": "Honduras",
	"HR": "Croatia",
	"HT": "Haiti",
	"HU": "Hungary",
	"ID": "Indonesia",
	"IE": "Ireland",
	"IL": "Israel",
	"IN": "India",
	"IQ": "Iraq",
	"IR": "Iran",
	"IS": "Iceland",
	"IT": "Italy",
	"JM": "Jamaica",
	"JO": "Jordan",
	"JP": "Japan",
	"KE": "Kenya",
	"KG": "Kyrgyzstan",
	"KH": "Cambodia",
	"KM": "Comoros",
	"KN": "Saint Kitts and Nevis",
	"KP": "North Korea",
	"KR": "South Korea",
	"KW": "Kuwait",
	"KZ": "Kazakhstan",
	"LA": "Laos",
	"LB": "Lebanon",
	"LC": "Saint Lucia",
	"LI": "Liechtenstein",
	"LK": "Sri Lanka",
	"LR": "Liberia",
	"LS": "Lesotho",
	"LT": "Lithuania",
	"LU": "Luxembourg",
	"LV": "Latvia",
	"LY": "Libya",
	"MA": "Morocco",
	"MC": "Monaco",
	"MD": "Moldova",
	"ME": "Montenegro",
	"MG": "Madagascar",
	"MK": "North Macedonia",
	"ML": "Mali",
	"MM": "Myanmar",
	"MN": "Mongolia",
	"MR": "Mauritania",
	"MT": "Malta",
	"MU": "Mauritius",
	"MV": "Maldives",
	"MW": "Malawi",
	"MX": "Mexico",
	"MY": "Malaysia",
	"MZ": "Mozambique",
	"NA": "Namibia",
	"NE": "Niger",
	"NG": "Nigeria",
	"NI": "Nicaragua",
	"NL": "Netherlands",
	"NO": "Norway",
	"NP": "Nepal",
	"NZ": "New Zealand",
	"OM": "Oman",
	"PA": "Panama",
	"PE": "Peru",
	"PG": "Papua New Guinea",
	"PH": "Philippines",
	"PK": "Pakistan",
	"PL": "Poland",
	"PR": "Puerto Rico",
	"PS": "Palestine",
	"PT": "Portugal",
	"PY": "Paraguay",
	"QA": "Qatar",
	"RO": "Romania",
	"RU": "Russia",
	"RW": "Rwanda",
	"SA": "Saudi Arabia",
	"SB": "Solomon Islands",
	"SC": "Seychelles",
	"SD": "Sudan",
	"SE": "Sweden",
	"SG": "Singapore",
	"SI": "Slovenia",
	"SK": "Slovakia",
	"SL": "Sierra Leone",
	"SM": "San Marino",
	"SN": "Senegal",
	"SO": "Somalia",
	"SR": "Suriname",
	"SS": "South Sudan",
	"SV": "El Salvador",
	"SY": "Syria",
	"SZ": "Eswatini",
	"TD": "Chad",
	"TG": "Togo",
	"TH": "Thailand",
	"TJ": "Tajikistan",
	"TL": "Timor-Leste",
	"TM": "Turkmenistan",
	"TN": "Tunisia",
	"TO": "Tonga",
	"TR": "Turkey",
	"TT": "Trinidad and Tobago",
	"TW": "Taiwan",
	"TZ": "Tanzania",
	"UA": "Ukraine",
	"UG": "Uganda",
	"US": "United States",
	"UY": "Uruguay",
	"UZ": "Uzbekistan",
	"VC": "Saint Vincent and the Grenadines",
	"VE": "Venezuela",
	"VN": "Vietnam",
	"VU": "Vanuatu",
	"WS": "Samoa",
	"YE": "Yemen",
	"ZA": "South Africa",
	"ZM": "Zambia",
	"ZW": "Zimbabwe",
}

func IsoToCountryName(code string) string {
	if name, ok := isoToCountry[strings.ToUpper(code)]; ok {
		return name
	}
	return ""
}

func CountryToIso(name string) string {
	lookup := strings.ToLower(name)
	for code, cname := range isoToCountry {
		if strings.ToLower(cname) == lookup {
			return code
		}
	}
	return ""
}

type Fetcher struct {
	store      peerStore
	client     *http.Client
	archiveURL string
	etag       string
}

type peerStore interface {
	BulkUpsertPeers([]store.PeerInput) error
}

func New(s *store.Store) *Fetcher {
	return &Fetcher{
		store:      s,
		client:     &http.Client{Timeout: archiveFetchTimeout},
		archiveURL: repoArchiveURL,
	}
}

func (f *Fetcher) FetchAndParse(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.archiveURL, nil)
	if err != nil {
		return fmt.Errorf("create archive request: %w", err)
	}
	if f.etag != "" {
		req.Header.Set("If-None-Match", f.etag)
	}
	// Keep the representation (and therefore its ETag) stable, and leave the
	// archive's gzip layer for the bounded reader below to validate.
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "yggpeers")

	log.Println("Fetching public-peers archive...")
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch archive: unexpected HTTP status %s", resp.Status)
	}
	if resp.ContentLength > maxArchiveBytes {
		return fmt.Errorf("fetch archive: compressed archive exceeds %d bytes", maxArchiveBytes)
	}

	compressed := &io.LimitedReader{R: resp.Body, N: maxArchiveBytes + 1}
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	peers, err := parsePeerArchive(gz)
	closeErr := gz.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("close archive: %w", closeErr)
	}
	if compressed.N == 0 {
		return fmt.Errorf("fetch archive: compressed archive exceeds %d bytes", maxArchiveBytes)
	}
	if len(peers) == 0 {
		return errors.New("archive contained no peers")
	}
	if err := f.store.BulkUpsertPeers(peers); err != nil {
		return fmt.Errorf("store peers: %w", err)
	}

	f.etag = resp.Header.Get("ETag")
	return nil
}

func parsePeerArchive(r io.Reader) ([]store.PeerInput, error) {
	var peers []store.PeerInput
	expanded := &io.LimitedReader{R: r, N: maxExpandedBytes + 1}
	tr := tar.NewReader(expanded)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if !validArchivePath(header.Name) {
			return nil, fmt.Errorf("archive contains invalid path %q", header.Name)
		}
		base := path.Base(header.Name)
		if !strings.HasSuffix(base, ".md") || strings.EqualFold(base, "README.md") {
			continue
		}
		if header.Size > maxPeerFileBytes {
			return nil, fmt.Errorf("archive file %q exceeds %d bytes", header.Name, maxPeerFileBytes)
		}

		parentDir := path.Base(path.Dir(header.Name))
		isNetwork := parentDir == "other"
		country := filenameToCountry(base)
		uris, err := parsePeerReader(tr)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", header.Name, err)
		}
		for _, uri := range uris {
			proto, host, port, err := parsePeerURI(uri)
			if err != nil {
				log.Printf("warn: parse URI %q: %v", uri, err)
				continue
			}
			if port == "" {
				continue
			}
			peers = append(peers, store.PeerInput{
				URI:       uri,
				Country:   country,
				Protocol:  proto,
				Host:      host,
				Port:      port,
				IsNetwork: isNetwork,
			})
		}
	}
	if _, err := io.Copy(io.Discard, expanded); err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	if expanded.N == 0 {
		return nil, fmt.Errorf("archive exceeds %d expanded bytes", maxExpandedBytes)
	}
	return peers, nil
}

func validArchivePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func parsePeerReader(r io.Reader) ([]string, error) {
	var uris []string
	sc := bufio.NewScanner(r)
	// Allow long markdown lines (default is 64 KiB).
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		for _, m := range peerURIRe.FindAllStringSubmatch(sc.Text(), -1) {
			uris = append(uris, strings.TrimSpace(m[1]))
		}
	}
	return uris, sc.Err()
}

func parsePeerURI(raw string) (proto, host, port string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", err
	}
	return u.Scheme, u.Hostname(), u.Port(), nil
}

func filenameToCountry(filename string) string {
	name := strings.TrimSuffix(filename, ".md")
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}
