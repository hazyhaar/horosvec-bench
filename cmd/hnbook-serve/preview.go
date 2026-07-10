package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Ce fichier ajoute le point d'accès de prévisualisation Open Graph du serveur de démo. Pour un
// item dont l'URL cible est externe (ex. lwn.net), le navigateur ne peut pas récupérer lui-même
// les métadonnées de la page (politique CORS) ; le serveur agit donc en mandataire borné et
// DURCI. Le durcissement est le cœur du dispositif : seul le schéma http(s) est admis, toute
// adresse IP privée ou spéciale est refusée AVANT connexion et RE-VALIDÉE au moment exact où la
// socket compose l'adresse (garde anti-rebinding DNS), les redirections sont bornées et
// revérifiées à chaque saut, la taille et le temps sont plafonnés, et seul du HTML est analysé.
// Aucune dépendance hors bibliothèque standard : l'extraction Open Graph est un scan borné.

const (
	// previewMaxURLLen borne la longueur de l'URL cible acceptée en paramètre (anti-abus).
	previewMaxURLLen = 2048
	// previewTimeout est le budget temps TOTAL d'une prévisualisation (résolution + connexion +
	// lecture), porté par le contexte de la requête sortante.
	previewTimeout = 5 * time.Second
	// previewMaxBody plafonne la taille de la réponse distante lue et analysée (512 Kio).
	previewMaxBody = 512 << 10
	// previewMaxRedirects borne le nombre de redirections suivies ; chaque saut est re-validé.
	previewMaxRedirects = 3
	// previewUserAgent identifie honnêtement le mandataire auprès de la page cible.
	previewUserAgent = "horosvec-demo-preview/1.0"
	// previewCacheTTL est la durée de validité d'une entrée en cache mémoire.
	previewCacheTTL = time.Hour
	// previewCacheMax borne le nombre d'entrées du cache (éviction au-delà).
	previewCacheMax = 2048
)

// previewResult est le corps JSON rendu par /api/preview. Tous les champs sont vides quand la
// métadonnée correspondante est absente. Error porte un motif bref en cas d'échec (la page se
// dégrade alors gracieusement vers l'affichage de l'hôte seul) ; il n'est jamais accompagné d'un
// statut 500, pour ne pas casser le panneau côté navigateur.
type previewResult struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Image       string `json:"image"`
	SiteName    string `json:"site_name"`
	URL         string `json:"url"`
	Error       string `json:"error,omitempty"`
}

// previewer porte le client HTTP durci et le cache mémoire des prévisualisations. Il est
// construit une seule fois par serveur (initialisation paresseuse), sans état de requête mutable
// hors le cache (protégé par son propre mutex).
type previewer struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]previewEntry
}

type previewEntry struct {
	res    previewResult
	expiry time.Time
}

// newPreviewer construit un previewer dont le client HTTP applique la garde anti-SSRF à deux
// niveaux : une fonction Control sur le dialer (re-validation de l'IP réellement composée, garde
// anti-rebinding) et un CheckRedirect qui rejoue la garde sur chaque URL de redirection.
func newPreviewer() *previewer {
	p := &previewer{cache: make(map[string]previewEntry)}
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: previewTimeout,
			Control: dialControlGuard,
		}).DialContext,
		TLSHandshakeTimeout:   previewTimeout,
		ResponseHeaderTimeout: previewTimeout,
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
	}
	p.client = &http.Client{
		Transport: transport,
		Timeout:   previewTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= previewMaxRedirects {
				return fmt.Errorf("trop de redirections (> %d)", previewMaxRedirects)
			}
			return validatePreviewURL(req.URL)
		},
	}
	return p
}

// dialControlGuard est appelé par le dialer APRÈS résolution DNS, avec l'adresse IP:port
// effectivement sur le point d'être composée. C'est le point de contrôle autoritaire contre le
// rebinding DNS : même si la résolution préalable a renvoyé une IP publique, une IP interne
// obtenue à la seconde résolution (celle du dial) est refusée ici, avant tout connect.
func dialControlGuard(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("adresse de connexion invalide: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("adresse de connexion non-IP: %s", host)
	}
	return validatePublicIP(ip)
}

// validatePublicIP refuse toute adresse qui n'est pas routable publiquement : loopback,
// lien-local (unicast et multicast, dont 169.254.169.254 des métadonnées cloud), privées
// (10/8, 172.16/12, 192.168/16, fc00::/7), non spécifiée, multicast, ainsi que les plages
// spéciales restantes (CGNAT 100.64/10, IETF 192.0.0/24, bench 198.18/15, réservé 240/4).
func validatePublicIP(ip net.IP) error {
	if ip == nil {
		return errors.New("ip absente")
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("ip non routable publiquement refusée: %s", ip)
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
			return fmt.Errorf("ip CGNAT refusée: %s", ip)
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24 IETF
			return fmt.Errorf("ip spéciale IETF refusée: %s", ip)
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15 benchmarking
			return fmt.Errorf("ip de banc refusée: %s", ip)
		case v4[0] >= 240: // 240.0.0.0/4 réservé
			return fmt.Errorf("ip réservée refusée: %s", ip)
		}
	}
	return nil
}

// validatePreviewURL vérifie le schéma (http/https uniquement), la présence d'un hôte, puis
// résout le nom et refuse la cible si une SEULE des IP résolues n'est pas publique. Cette
// pré-vérification donne une erreur claire tôt ; la garde autoritaire reste le dialControlGuard
// (qui rejoue au moment du connect, couvrant le rebinding entre cette résolution et le dial).
func validatePreviewURL(u *url.URL) error {
	if u == nil {
		return errors.New("url absente")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("schéma non autorisé: %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("hôte absent")
	}
	// Hôte déjà une IP littérale : valider directement.
	if ip := net.ParseIP(host); ip != nil {
		return validatePublicIP(ip)
	}
	ctx, cancel := context.WithTimeout(context.Background(), previewTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("résolution DNS échouée: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("aucune adresse pour %q", host)
	}
	for _, ip := range ips {
		if err := validatePublicIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// fetch récupère et analyse la page cible sous toutes les bornes de durcissement. Toute défaite
// (refus SSRF, timeout, non-HTML, statut non 200) revient en previewResult{Error: ...}, jamais
// en erreur remontée au handler HTTP — la dégradation reste gracieuse côté navigateur.
func (p *previewer) fetch(ctx context.Context, raw string) previewResult {
	res := previewResult{URL: raw}

	u, err := url.Parse(raw)
	if err != nil {
		res.Error = "invalid url"
		return res
	}
	if err := validatePreviewURL(u); err != nil {
		res.Error = "target refused"
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		res.Error = "invalid request"
		return res
	}
	req.Header.Set("User-Agent", previewUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := p.client.Do(req)
	if err != nil {
		res.Error = "target unreachable"
		return res
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		res.Error = fmt.Sprintf("status %d", resp.StatusCode)
		return res
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/html") {
		res.Error = "non-HTML content"
		return res
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, previewMaxBody))
	if err != nil {
		res.Error = "read interrupted"
		return res
	}

	og := parseOpenGraph(body)
	og.URL = raw
	return og
}

var (
	reMetaTag     = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	reMetaAttr    = regexp.MustCompile(`(?is)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	reTitleTag    = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title>`)
	reHeadCloseTg = regexp.MustCompile(`(?is)</head>`)
)

// parseOpenGraph extrait les métadonnées Open Graph d'un corps HTML SANS dépendance externe :
// il scanne les balises <meta> (ordre d'attributs variable, guillemets simples ou doubles), lit
// property/name et content, et replie sur <title> et <meta name="description"> quand og:title /
// og:description manquent. Les valeurs sont décodées de leurs entités HTML (html.UnescapeString).
// La robustesse prime sur l'exhaustivité : ce qui n'est pas reconnu est simplement ignoré.
func parseOpenGraph(body []byte) previewResult {
	// Borne le scan au <head> lorsqu'il est délimité (les métadonnées y vivent), sinon au corps
	// déjà plafonné à previewMaxBody.
	scan := body
	if loc := reHeadCloseTg.FindIndex(body); loc != nil {
		scan = body[:loc[1]]
	}
	s := string(scan)

	var res previewResult
	var metaDesc, htmlTitle string

	for _, tag := range reMetaTag.FindAllString(s, -1) {
		var key, content string
		hasContent := false
		for _, m := range reMetaAttr.FindAllStringSubmatch(tag, -1) {
			name := strings.ToLower(m[1])
			val := m[3]
			if val == "" {
				val = m[4]
			}
			if val == "" {
				val = m[5]
			}
			switch name {
			case "property", "name":
				key = strings.ToLower(strings.TrimSpace(val))
			case "content":
				content = val
				hasContent = true
			}
		}
		if key == "" || !hasContent {
			continue
		}
		content = html.UnescapeString(content)
		switch key {
		case "og:title":
			if res.Title == "" {
				res.Title = content
			}
		case "og:description":
			if res.Description == "" {
				res.Description = content
			}
		case "og:image", "og:image:url", "og:image:secure_url":
			if res.Image == "" {
				res.Image = content
			}
		case "og:site_name":
			if res.SiteName == "" {
				res.SiteName = content
			}
		case "description":
			if metaDesc == "" {
				metaDesc = content
			}
		}
	}

	if m := reTitleTag.FindStringSubmatch(s); m != nil {
		htmlTitle = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	if res.Title == "" {
		res.Title = htmlTitle
	}
	if res.Description == "" {
		res.Description = metaDesc
	}
	return res
}

// lookup restitue une entrée de cache non expirée, ou faux.
func (p *previewer) lookup(key string) (previewResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.cache[key]
	if !ok || time.Now().After(e.expiry) {
		return previewResult{}, false
	}
	return e.res, true
}

// store insère une entrée avec TTL et borne la taille du cache : purge d'abord les entrées
// expirées, puis, si le plafond est encore atteint, évince une entrée arbitraire (éviction
// simple suffisante pour un cache anti-abus, pas un cache de performance à politique fine).
func (p *previewer) store(key string, res previewResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if len(p.cache) >= previewCacheMax {
		for k, e := range p.cache {
			if now.After(e.expiry) {
				delete(p.cache, k)
			}
		}
		if len(p.cache) >= previewCacheMax {
			for k := range p.cache { // évince une entrée arbitraire
				delete(p.cache, k)
				break
			}
		}
	}
	p.cache[key] = previewEntry{res: res, expiry: now.Add(previewCacheTTL)}
}
