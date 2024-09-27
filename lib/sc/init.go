package sc

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maid-zone/soundcloak/lib/cfg"
	"github.com/valyala/fasthttp"
)

var clientIdCache struct {
	ClientID       []byte
	ClientIDString string
	Version        []byte
	NextCheck      time.Time
}

type cached[T any] struct {
	Value   T
	Expires time.Time
}

var httpc = fasthttp.HostClient{
	Addr:          "api-v2.soundcloud.com:443",
	IsTLS:         true,
	DialDualStack: true,
	Dial:          (&fasthttp.TCPDialer{DNSCacheDuration: cfg.DNSCacheTTL}).Dial,
	//MaxIdleConnDuration: 1<<63 - 1,
}

var usersCache = map[string]cached[User]{}
var usersCacheLock = &sync.RWMutex{}

var tracksCache = map[string]cached[Track]{}
var tracksCacheLock = &sync.RWMutex{}

var playlistsCache = map[string]cached[Playlist]{}
var playlistsCacheLock = &sync.RWMutex{}

var verRegex = regexp.MustCompile(`(?m)^<script>window\.__sc_version="([0-9]{10})"</script>$`)
var scriptsRegex = regexp.MustCompile(`(?m)^<script crossorigin src="(https://a-v2\.sndcdn\.com/assets/.+\.js)"></script>$`)
var clientIdRegex = regexp.MustCompile(`\("client_id=([A-Za-z0-9]{32})"\)`)
var ErrVersionNotFound = errors.New("version not found")
var ErrScriptNotFound = errors.New("script not found")
var ErrIDNotFound = errors.New("clientid not found")
var ErrKindNotCorrect = errors.New("entity of incorrect kind")
var ErrIncompatibleStream = errors.New("incompatible stream")
var ErrNoURL = errors.New("no url")

// inspired by github.com/imputnet/cobalt (mostly stolen lol)
func GetClientID() (string, error) {
	if clientIdCache.NextCheck.After(time.Now()) {
		return clientIdCache.ClientIDString, nil
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI("https://soundcloud.com/h") // 404 page
	req.Header.Set("User-Agent", cfg.UserAgent)   // the connection is stuck with fasthttp useragent lol, maybe randomly select from a list of browser useragents in the future? low priority for now
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err := fasthttp.Do(req, resp)
	if err != nil {
		return "", err
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	//fmt.Println(string(data), err)

	res := verRegex.FindSubmatch(data)
	if len(res) != 2 {
		return "", ErrVersionNotFound
	}

	if bytes.Equal(res[1], clientIdCache.Version) {
		return clientIdCache.ClientIDString, nil
	}

	ver := res[1]

	scripts := scriptsRegex.FindAllSubmatch(data, -1)
	if len(scripts) == 0 {
		return "", ErrScriptNotFound
	}

	for _, scr := range scripts {
		if len(scr) != 2 {
			continue
		}

		req.SetRequestURIBytes(scr[1])

		err = fasthttp.Do(req, resp)
		if err != nil {
			continue
		}

		data, err = resp.BodyUncompressed()
		if err != nil {
			data = resp.Body()
		}

		res = clientIdRegex.FindSubmatch(data)
		if len(res) != 2 {
			continue
		}

		clientIdCache.ClientID = res[1]
		clientIdCache.ClientIDString = string(res[1])
		clientIdCache.Version = ver
		clientIdCache.NextCheck = time.Now().Add(cfg.ClientIDTTL)
		return clientIdCache.ClientIDString, nil
	}

	return "", ErrIDNotFound
}

func DoWithRetry(req *fasthttp.Request, resp *fasthttp.Response) (err error) {
	for i := 0; i < 5; i++ {
		err = httpc.Do(req, resp)
		if err == nil {
			return nil
		}

		if !os.IsTimeout(err) && err != fasthttp.ErrTimeout {
			return
		}
	}

	return
}

func Resolve(path string, out any) error {
	cid, err := GetClientID()
	if err != nil {
		return err
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI("https://api-v2.soundcloud.com/resolve?url=https%3A%2F%2Fsoundcloud.com%2F" + url.QueryEscape(path) + "&client_id=" + cid)
	req.Header.Set("User-Agent", cfg.UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err = DoWithRetry(req, resp)
	if err != nil {
		return err
	}

	if resp.StatusCode() != 200 {
		return fmt.Errorf("resolve: got status code %d", resp.StatusCode())
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	return cfg.JSON.Unmarshal(data, out)
}

func GetUser(permalink string) (User, error) {
	usersCacheLock.RLock()
	if cell, ok := usersCache[permalink]; ok && cell.Expires.After(time.Now()) {
		usersCacheLock.RUnlock()
		return cell.Value, nil
	}

	usersCacheLock.RUnlock()

	var u User
	err := Resolve(permalink, &u)
	if err != nil {
		return u, err
	}

	if u.Kind != "user" {
		err = ErrKindNotCorrect
		return u, err
	}

	u.Fix()

	usersCacheLock.Lock()
	usersCache[permalink] = cached[User]{Value: u, Expires: time.Now().Add(cfg.UserTTL)}
	usersCacheLock.Unlock()

	return u, err
}

func GetTrack(permalink string) (Track, error) {
	tracksCacheLock.RLock()
	if cell, ok := tracksCache[permalink]; ok && cell.Expires.After(time.Now()) {
		tracksCacheLock.RUnlock()
		return cell.Value, nil
	}
	tracksCacheLock.RUnlock()

	var u Track
	err := Resolve(permalink, &u)
	if err != nil {
		return u, err
	}

	if u.Kind != "track" {
		return u, ErrKindNotCorrect
	}

	u.Fix()

	tracksCacheLock.Lock()
	tracksCache[permalink] = cached[Track]{Value: u, Expires: time.Now().Add(cfg.TrackTTL)}
	tracksCacheLock.Unlock()

	return u, nil
}

func (p *Paginated[T]) Proceed() error {
	cid, err := GetClientID()
	if err != nil {
		return err
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI(p.Next + "&client_id=" + cid)
	req.Header.Set("User-Agent", cfg.UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err = DoWithRetry(req, resp)
	if err != nil {
		return err
	}

	if resp.StatusCode() != 200 {
		return fmt.Errorf("paginated.proceed: got status code %d", resp.StatusCode())
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	return cfg.JSON.Unmarshal(data, p)
}

func (u User) GetTracks(args string) (*Paginated[Track], error) {
	p := Paginated[Track]{
		Next: "https://api-v2.soundcloud.com/users/" + u.ID + "/tracks" + args,
	}

	err := p.Proceed()
	if err != nil {
		return nil, err
	}

	for _, u := range p.Collection {
		u.Fix()
	}

	return &p, nil
}

func (t Track) GetStream() (string, error) {
	cid, err := GetClientID()
	if err != nil {
		return "", err
	}

	tr := t.Media.SelectCompatible()
	if tr == nil {
		return "", ErrIncompatibleStream
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI(tr.URL + "?client_id=" + cid + "&track_authorization=" + t.Authorization)
	req.Header.Set("User-Agent", cfg.UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err = DoWithRetry(req, resp)
	if err != nil {
		return "", err
	}

	if resp.StatusCode() != 200 {
		return "", fmt.Errorf("resolve: got status code %d", resp.StatusCode())
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	var s Stream
	err = cfg.JSON.Unmarshal(data, &s)
	if err != nil {
		return "", err
	}

	if s.URL == "" {
		return "", ErrNoURL
	}

	return s.URL, nil
}

func SearchTracks(args string) (*Paginated[*Track], error) {
	cid, err := GetClientID()
	if err != nil {
		return nil, err
	}

	p := Paginated[*Track]{Next: "https://api-v2.soundcloud.com/search/tracks" + args + "&client_id=" + cid}
	err = p.Proceed()
	if err != nil {
		return nil, err
	}

	for _, u := range p.Collection {
		u.Fix()
	}

	return &p, nil
}

func SearchUsers(args string) (*Paginated[*User], error) {
	cid, err := GetClientID()
	if err != nil {
		return nil, err
	}

	p := Paginated[*User]{Next: "https://api-v2.soundcloud.com/search/users" + args + "&client_id=" + cid}
	err = p.Proceed()
	if err != nil {
		return nil, err
	}

	for _, u := range p.Collection {
		u.Fix()
	}

	return &p, nil
}

func SearchPlaylists(args string) (*Paginated[*Playlist], error) {
	cid, err := GetClientID()
	if err != nil {
		return nil, err
	}

	p := Paginated[*Playlist]{Next: "https://api-v2.soundcloud.com/search/playlists" + args + "&client_id=" + cid}
	err = p.Proceed()
	if err != nil {
		return nil, err
	}

	for _, u := range p.Collection {
		u.Fix(false)
	}

	return &p, nil
}

func GetPlaylist(permalink string) (Playlist, error) {
	playlistsCacheLock.RLock()
	if cell, ok := playlistsCache[permalink]; ok && cell.Expires.After(time.Now()) {
		playlistsCacheLock.RUnlock()
		return cell.Value, nil
	}
	playlistsCacheLock.RUnlock()

	var u Playlist
	err := Resolve(permalink, &u)
	if err != nil {
		return u, err
	}

	if u.Kind != "playlist" {
		return u, ErrKindNotCorrect
	}

	err = u.Fix(true)
	if err != nil {
		return u, err
	}

	playlistsCacheLock.Lock()
	playlistsCache[permalink] = cached[Playlist]{Value: u, Expires: time.Now().Add(cfg.PlaylistTTL)}
	playlistsCacheLock.Unlock()

	return u, nil
}

func (u *Playlist) Fix(cached bool) error {
	if cached {
		for _, t := range u.Tracks {
			t.Fix()
		}

		err := u.GetMissingTracks()
		if err != nil {
			return err
		}
	}

	u.Artwork = strings.Replace(u.Artwork, "-large.", "-t200x200.", 1)
	return nil
}

func TagListParser(taglist string) (res []string) {
	inString := false
	cur := []rune{}
	for i, c := range taglist {
		if c == '"' {
			if i == len(taglist)-1 {
				res = append(res, string(cur))
				return
			}

			inString = !inString
			continue
		}

		if !inString && c == ' ' {
			res = append(res, string(cur))
			cur = []rune{}
			continue
		}

		cur = append(cur, c)
	}

	return
}

func (t Track) FormatDescription() string {
	desc := t.Description
	if t.Description != "" {
		desc += "\n\n"
	}

	desc += strconv.FormatInt(t.Likes, 10) + " ❤️ | " + strconv.FormatInt(t.Played, 10) + " ▶️"
	if t.Genre != "" {
		desc += "\nGenre: " + t.Genre
	}
	desc += "\nCreated: " + t.CreatedAt
	desc += "\nLast modified: " + t.LastModified
	if len(t.TagList) != 0 {
		desc += "\nTags: " + strings.Join(TagListParser(t.TagList), ", ")
	}

	return desc
}

func (u User) FormatDescription() string {
	desc := u.Description
	if u.Description != "" {
		desc += "\n\n"
	}

	desc += strconv.FormatInt(u.Followers, 10) + " followers | " + strconv.FormatInt(u.Following, 10) + " following"
	desc += "\n" + strconv.FormatInt(u.Tracks, 10) + " tracks | " + strconv.FormatInt(u.Playlists, 10) + " playlists"
	desc += "\nCreated: " + u.CreatedAt
	desc += "\nLast modified: " + u.LastModified

	return desc
}

func (u User) FormatUsername() string {
	res := u.Username
	if u.Verified {
		res += " ☑️"
	}

	return res
}

func (p Playlist) FormatDescription() string {
	desc := p.Description
	if p.Description != "" {
		desc += "\n\n"
	}

	desc += strconv.FormatInt(int64(len(p.Tracks)), 10) + " tracks"
	desc += "\n" + strconv.FormatInt(p.Likes, 10) + " ❤️"
	desc += "\nCreated: " + p.CreatedAt
	desc += "\nLast modified: " + p.LastModified
	if len(p.TagList) != 0 {
		desc += "\nTags: " + strings.Join(TagListParser(p.TagList), ", ")
	}

	return desc
}

type MissingTrack struct {
	ID    string
	Index int
}

func GetTracks(ids string) ([]*Track, error) {
	cid, err := GetClientID()
	if err != nil {
		return nil, err
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI("https://api-v2.soundcloud.com/tracks?ids=" + ids + "&client_id=" + cid)
	req.Header.Set("User-Agent", cfg.UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err = DoWithRetry(req, resp)
	if err != nil {
		return nil, err
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	var res []*Track
	err = cfg.JSON.Unmarshal(data, &res)
	for _, t := range res {
		t.Fix()
	}
	return res, err
}

func JoinMissingTracks(missing []MissingTrack) (st string) {
	for i, track := range missing {
		st += track.ID
		if i != len(missing)-1 {
			st += ","
		}
	}
	return
}

func GetMissingTracks(missing []MissingTrack) (res []*Track, next []MissingTrack, err error) {
	if len(missing) > 50 {
		next = missing[50:]
		missing = missing[:50]
	}

	res, err = GetTracks(JoinMissingTracks(missing))
	return
}

func GetNextMissingTracks(raw string) (res []*Track, next []string, err error) {
	missing := strings.Split(raw, ",")
	if len(missing) > 50 {
		next = missing[50:]
		missing = missing[:50]
	}

	res, err = GetTracks(strings.Join(missing, ","))
	return
}

func (p *Playlist) GetMissingTracks() error {
	missing := []MissingTrack{}
	for i, track := range p.Tracks {
		if track.Title == "" {
			//fmt.Println(track.ID)
			missing = append(missing, MissingTrack{ID: track.ID, Index: i})
		}
	}

	res, next, err := GetMissingTracks(missing)
	if err != nil {
		return err
	}

	for _, oldTrack := range missing {
		for _, newTrack := range res {
			if newTrack.ID == oldTrack.ID {
				p.Tracks[oldTrack.Index] = newTrack
			}
		}
	}

	p.MissingTracks = JoinMissingTracks(next)

	return nil
}

func (u *Track) Fix() {
	u.Artwork = strings.Replace(u.Artwork, "-large.", "-t200x200.", 1)
	//fmt.Println(u.ID, u.IDint)
	if u.ID == "" {
		u.ID = strconv.FormatInt(u.IDint, 10)
	} else {
		ls := strings.Split(u.ID, ":")
		u.ID = ls[len(ls)-1]
	}
}

func (u *User) Fix() {
	u.Avatar = strings.Replace(u.Avatar, "-large.", "-t200x200.", 1)
	ls := strings.Split(u.ID, ":")
	u.ID = ls[len(ls)-1]
}

// could probably make a generic function, whatever
func init() {
	go func() {
		ticker := time.NewTicker(cfg.UserTTL)
		for range ticker.C {
			usersCacheLock.Lock()

			for key, val := range usersCache {
				if val.Expires.Before(time.Now()) {
					delete(usersCache, key)
				}
			}

			usersCacheLock.Unlock()
		}
	}()

	go func() {
		ticker := time.NewTicker(cfg.TrackTTL)
		for range ticker.C {
			tracksCacheLock.Lock()

			for key, val := range tracksCache {
				if val.Expires.Before(time.Now()) {
					delete(tracksCache, key)
				}
			}

			tracksCacheLock.Unlock()
		}
	}()

	go func() {
		ticker := time.NewTicker(cfg.PlaylistTTL)
		for range ticker.C {
			playlistsCacheLock.Lock()

			for key, val := range playlistsCache {
				if val.Expires.Before(time.Now()) {
					delete(playlistsCache, key)
				}
			}

			playlistsCacheLock.Unlock()
		}
	}()
}
