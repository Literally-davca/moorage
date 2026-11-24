package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"moorage/internal/backupcrypt"
	"moorage/internal/store"
)

type remoteBackupConfig struct {
	Enabled        bool   `json:"enabled"`
	Provider       string `json:"provider"` // "off" | "s3" | "webdav"
	Schedule       string `json:"schedule"` // "HH:MM"
	Retention      int    `json:"retention"`
	BackupPassword string `json:"backupPassword,omitempty"`

	S3Endpoint     string `json:"s3Endpoint"`
	S3Bucket       string `json:"s3Bucket"`
	S3Region       string `json:"s3Region"`
	S3AccessKey    string `json:"s3AccessKey"`
	S3SecretKey    string `json:"s3SecretKey,omitempty"`
	S3Prefix       string `json:"s3Prefix"`
	S3AllowPrivate bool   `json:"s3AllowPrivate"`

	WebDAVURL          string `json:"webdavUrl"`
	WebDAVUsername     string `json:"webdavUsername"`
	WebDAVPassword     string `json:"webdavPassword,omitempty"`
	WebDAVAllowPrivate bool   `json:"webdavAllowPrivate"`
}

type remoteBackupLastStatus struct {
	At        time.Time `json:"at"`
	OK        bool      `json:"ok"`
	Target    string    `json:"target,omitempty"`
	SizeBytes int64     `json:"sizeBytes,omitempty"`
	Pruned    int       `json:"pruned,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type remoteBackupDTO struct {
	Enabled           bool                    `json:"enabled"`
	Provider          string                  `json:"provider"`
	Schedule          string                  `json:"schedule"`
	Retention         int                     `json:"retention"`
	BackupPasswordSet bool                    `json:"backupPasswordSet"`
	S3Endpoint        string                  `json:"s3Endpoint"`
	S3Bucket          string                  `json:"s3Bucket"`
	S3Region          string                  `json:"s3Region"`
	S3AccessKey       string                  `json:"s3AccessKey"`
	S3SecretKeySet    bool                    `json:"s3SecretKeySet"`
	S3Prefix          string                  `json:"s3Prefix"`
	S3AllowPrivate    bool                    `json:"s3AllowPrivate"`
	WebDAVURL         string                  `json:"webdavUrl"`
	WebDAVUsername    string                  `json:"webdavUsername"`
	WebDAVPasswordSet bool                    `json:"webdavPasswordSet"`
	WebDAVAllowPrivate bool                   `json:"webdavAllowPrivate"`
	FromEnv           map[string]bool         `json:"fromEnv"`
	Last              *remoteBackupLastStatus `json:"last,omitempty"`
}

func (s *Server) effectiveRemoteBackup(ctx context.Context) (remoteBackupConfig, map[string]bool) {
	cfg := remoteBackupConfig{
		Provider:  "off",
		Schedule:  "04:00",
		Retention: 7,
		S3Region:  "auto",
		S3Prefix:  "moorage/",
	}
	if s.store != nil {
		if raw, _ := s.store.GetSetting(ctx, store.SettingBackupRemoteJSON); raw != "" {
			_ = json.Unmarshal([]byte(raw), &cfg)
		}
	}
	fromEnv := map[string]bool{}
	envStr := func(envKey, field string, dst *string) {
		if v := strings.TrimSpace(s.cfg.Env[envKey]); v != "" {
			*dst = v
			fromEnv[field] = true
		}
	}
	envStr("MOORAGE_BACKUP_PASSWORD", "backupPassword", &cfg.BackupPassword)
	envStr("MOORAGE_BACKUP_SCHEDULE", "schedule", &cfg.Schedule)
	if v := strings.TrimSpace(s.cfg.Env["MOORAGE_BACKUP_RETENTION"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Retention = n
			fromEnv["retention"] = true
		}
	}
	envStr("MOORAGE_BACKUP_S3_ENDPOINT", "s3Endpoint", &cfg.S3Endpoint)
	envStr("MOORAGE_BACKUP_S3_BUCKET", "s3Bucket", &cfg.S3Bucket)
	envStr("MOORAGE_BACKUP_S3_REGION", "s3Region", &cfg.S3Region)
	envStr("MOORAGE_BACKUP_S3_ACCESS_KEY", "s3AccessKey", &cfg.S3AccessKey)
	envStr("MOORAGE_BACKUP_S3_SECRET_KEY", "s3SecretKey", &cfg.S3SecretKey)
	envStr("MOORAGE_BACKUP_S3_PREFIX", "s3Prefix", &cfg.S3Prefix)
	if strings.EqualFold(strings.TrimSpace(s.cfg.Env["MOORAGE_BACKUP_S3_ALLOW_PRIVATE"]), "true") {
		cfg.S3AllowPrivate = true
	}
	envStr("MOORAGE_BACKUP_WEBDAV_URL", "webdavUrl", &cfg.WebDAVURL)
	envStr("MOORAGE_BACKUP_WEBDAV_USER", "webdavUsername", &cfg.WebDAVUsername)
	envStr("MOORAGE_BACKUP_WEBDAV_PASSWORD", "webdavPassword", &cfg.WebDAVPassword)
	if strings.EqualFold(strings.TrimSpace(s.cfg.Env["MOORAGE_BACKUP_WEBDAV_ALLOW_PRIVATE"]), "true") {
		cfg.WebDAVAllowPrivate = true
	}
	if fromEnv["s3Endpoint"] && cfg.S3Bucket != "" {
		cfg.Provider = "s3"
		cfg.Enabled = true
	} else if fromEnv["webdavUrl"] {
		cfg.Provider = "webdav"
		cfg.Enabled = true
	}
	if cfg.Schedule == "" {
		cfg.Schedule = "04:00"
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 7
	}
	if cfg.S3Region == "" {
		cfg.S3Region = "auto"
	}
	return cfg, fromEnv
}

func (s *Server) getRemoteBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, fromEnv := s.effectiveRemoteBackup(ctx)
	var last *remoteBackupLastStatus
	if raw, _ := s.store.GetSetting(ctx, store.SettingBackupLastStatus); raw != "" {
		var st remoteBackupLastStatus
		if json.Unmarshal([]byte(raw), &st) == nil {
			last = &st
		}
	}
	writeJSON(w, http.StatusOK, remoteBackupDTO{
		Enabled:            cfg.Enabled,
		Provider:           cfg.Provider,
		Schedule:           cfg.Schedule,
		Retention:          cfg.Retention,
		BackupPasswordSet:  cfg.BackupPassword != "",
		S3Endpoint:         cfg.S3Endpoint,
		S3Bucket:           cfg.S3Bucket,
		S3Region:           cfg.S3Region,
		S3AccessKey:        cfg.S3AccessKey,
		S3SecretKeySet:     cfg.S3SecretKey != "",
		S3Prefix:           cfg.S3Prefix,
		S3AllowPrivate:     cfg.S3AllowPrivate,
		WebDAVURL:          cfg.WebDAVURL,
		WebDAVUsername:     cfg.WebDAVUsername,
		WebDAVPasswordSet:  cfg.WebDAVPassword != "",
		WebDAVAllowPrivate: cfg.WebDAVAllowPrivate,
		FromEnv:            fromEnv,
		Last:               last,
	})
}

func (s *Server) putRemoteBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled            bool   `json:"enabled"`
		Provider           string `json:"provider"`
		Schedule           string `json:"schedule"`
		Retention          int    `json:"retention"`
		BackupPassword     string `json:"backupPassword"`
		S3Endpoint         string `json:"s3Endpoint"`
		S3Bucket           string `json:"s3Bucket"`
		S3Region           string `json:"s3Region"`
		S3AccessKey        string `json:"s3AccessKey"`
		S3SecretKey        string `json:"s3SecretKey"`
		S3Prefix           string `json:"s3Prefix"`
		S3AllowPrivate     bool   `json:"s3AllowPrivate"`
		WebDAVURL          string `json:"webdavUrl"`
		WebDAVUsername     string `json:"webdavUsername"`
		WebDAVPassword     string `json:"webdavPassword"`
		WebDAVAllowPrivate bool   `json:"webdavAllowPrivate"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	cur, fromEnv := s.effectiveRemoteBackup(ctx)
	if req.Provider != "off" && req.Provider != "s3" && req.Provider != "webdav" {
		writeErr(w, http.StatusBadRequest, "Provider must be off, s3 or webdav.")
		return
	}
	sched := strings.TrimSpace(req.Schedule)
	if sched == "" {
		sched = "04:00"
	}
	if len(sched) != 5 || sched[2] != ':' {
		writeErr(w, http.StatusBadRequest, "Backup schedule must be HH:MM.")
		return
	}
	ret := req.Retention
	if ret <= 0 {
		ret = 7
	}
	if ret > 365 {
		ret = 365
	}
	next := cur
	next.Enabled = req.Enabled && req.Provider != "off"
	next.Provider = req.Provider
	if !fromEnv["schedule"] {
		next.Schedule = sched
	}
	if !fromEnv["retention"] {
		next.Retention = ret
	}
	if !fromEnv["backupPassword"] && req.BackupPassword != "" {
		if err := backupcrypt.CheckPassword(req.BackupPassword); err != nil {
			writeErr(w, http.StatusBadRequest, capitalize(err.Error())+".")
			return
		}
		next.BackupPassword = req.BackupPassword
	}
	if !fromEnv["s3Endpoint"] {
		next.S3Endpoint = strings.TrimRight(strings.TrimSpace(req.S3Endpoint), "/")
	}
	if !fromEnv["s3Bucket"] {
		next.S3Bucket = strings.TrimSpace(req.S3Bucket)
	}
	if !fromEnv["s3Region"] {
		next.S3Region = strings.TrimSpace(req.S3Region)
	}
	if !fromEnv["s3AccessKey"] {
		next.S3AccessKey = strings.TrimSpace(req.S3AccessKey)
	}
	if !fromEnv["s3SecretKey"] && req.S3SecretKey != "" {
		next.S3SecretKey = req.S3SecretKey
	}
	if !fromEnv["s3Prefix"] {
		next.S3Prefix = strings.TrimLeft(strings.TrimSpace(req.S3Prefix), "/")
	}
	next.S3AllowPrivate = req.S3AllowPrivate

	if !fromEnv["webdavUrl"] {
		next.WebDAVURL = strings.TrimRight(strings.TrimSpace(req.WebDAVURL), "/")
	}
	if !fromEnv["webdavUsername"] {
		next.WebDAVUsername = strings.TrimSpace(req.WebDAVUsername)
	}
	if !fromEnv["webdavPassword"] && req.WebDAVPassword != "" {
		next.WebDAVPassword = req.WebDAVPassword
	}
	next.WebDAVAllowPrivate = req.WebDAVAllowPrivate

	b, _ := json.Marshal(next)
	if err := s.store.SetSetting(ctx, store.SettingBackupRemoteJSON, string(b)); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, "remote-backup")
	s.getRemoteBackup(w, r)
}

func (s *Server) runRemoteBackupNow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	st, err := s.executeRemoteBackup(ctx)
	setAuditTarget(r, st.Target)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) executeRemoteBackup(ctx context.Context) (remoteBackupLastStatus, error) {
	cfg, _ := s.effectiveRemoteBackup(ctx)
	now := time.Now().UTC()
	st := remoteBackupLastStatus{At: now}
	record := func(err error) (remoteBackupLastStatus, error) {
		if err != nil {
			st.OK = false
			st.Error = err.Error()
		} else {
			st.OK = true
		}
		if b, merr := json.Marshal(st); merr == nil && s.store != nil {
			_ = s.store.SetSetting(ctx, store.SettingBackupLastStatus, string(b))
		}
		return st, err
	}

	if cfg.Provider == "off" || cfg.Provider == "" {
		return record(errors.New("Choose S3 or WebDAV before running a remote backup."))
	}
	if err := backupcrypt.CheckPassword(cfg.BackupPassword); err != nil {
		return record(errors.New("Set a backup encryption password (8+ characters) first."))
	}

	tmpDir, err := os.MkdirTemp(s.cfg.DataDir, "export-remote-")
	if err != nil {
		return record(err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "moorage.db")
	if err := s.store.BackupTo(ctx, dbPath); err != nil {
		return record(err)
	}
	archiveName := fmt.Sprintf("moorage-backup-%s.mbk", now.Format("20060102-150405"))
	archivePath := filepath.Join(tmpDir, archiveName)
	f, err := os.OpenFile(archivePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return record(err)
	}
	enc, err := backupcrypt.NewWriter(f, cfg.BackupPassword)
	if err != nil {
		f.Close()
		return record(err)
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	if err := s.writeBackup(ctx, tw, tmpDir, dbPath); err != nil {
		f.Close()
		return record(err)
	}
	if err := tw.Close(); err != nil {
		f.Close()
		return record(err)
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return record(err)
	}
	if err := enc.Close(); err != nil {
		f.Close()
		return record(err)
	}
	fi, err := f.Stat()
	f.Close()
	if err != nil {
		return record(err)
	}
	st.SizeBytes = fi.Size()

	switch cfg.Provider {
	case "s3":
		st.Target = fmt.Sprintf("s3://%s/%s%s", cfg.S3Bucket, cfg.S3Prefix, archiveName)
		pruned, err := uploadAndPruneS3(ctx, cfg, archivePath, archiveName)
		st.Pruned = pruned
		return record(err)
	case "webdav":
		st.Target = cfg.WebDAVURL + "/" + archiveName
		pruned, err := uploadAndPruneWebDAV(ctx, cfg, archivePath, archiveName)
		st.Pruned = pruned
		return record(err)
	default:
		return record(errors.New("Unsupported backup provider."))
	}
}

// ---- S3 path-style client with AWS Signature V4 (works with S3, Garage, R2, MinIO) ----

func uploadAndPruneS3(ctx context.Context, cfg remoteBackupConfig, filePath, fileName string) (int, error) {
	if cfg.S3Endpoint == "" || cfg.S3Bucket == "" || cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		return 0, errors.New("S3 endpoint, bucket, access key and secret key are required.")
	}
	if err := checkHTTPURL(cfg.S3Endpoint, true); err != nil {
		return 0, fmt.Errorf("S3 endpoint: %w", err)
	}
	payload, err := os.ReadFile(filePath)
	if err != nil {
		return 0, err
	}
	c := guardedClient(3*time.Minute, cfg.S3AllowPrivate)
	defer c.CloseIdleConnections()

	prefix := cfg.S3Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	objKey := prefix + fileName
	if err := s3Request(ctx, c, cfg, http.MethodPut, "/"+cfg.S3Bucket+"/"+objKey, "", payload, nil); err != nil {
		return 0, fmt.Errorf("S3 upload: %w", err)
	}

	if cfg.Retention <= 0 {
		return 0, nil
	}
	q := url.Values{}
	q.Set("list-type", "2")
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	var listBody []byte
	if err := s3Request(ctx, c, cfg, http.MethodGet, "/"+cfg.S3Bucket, q.Encode(), nil, &listBody); err != nil {
		return 0, nil // upload succeeded; list failure does not fail the backup
	}
	var parsed struct {
		Contents []struct {
			Key          string `xml:"Key"`
			LastModified string `xml:"LastModified"`
		} `xml:"Contents"`
	}
	if xml.Unmarshal(listBody, &parsed) != nil {
		return 0, nil
	}
	var keys []string
	for _, it := range parsed.Contents {
		if strings.HasSuffix(it.Key, ".mbk") && strings.Contains(path.Base(it.Key), "moorage-backup-") {
			keys = append(keys, it.Key)
		}
	}
	sort.Strings(keys) // timestamped names sort oldest -> newest
	pruned := 0
	for len(keys) > cfg.Retention {
		old := keys[0]
		keys = keys[1:]
		if s3Request(ctx, c, cfg, http.MethodDelete, "/"+cfg.S3Bucket+"/"+old, "", nil, nil) == nil {
			pruned++
		}
	}
	return pruned, nil
}

func s3Request(ctx context.Context, c *http.Client, cfg remoteBackupConfig, method, urlPath, rawQuery string, body []byte, respOut *[]byte) error {
	base, err := url.Parse(cfg.S3Endpoint)
	if err != nil {
		return err
	}
	fullPath := strings.TrimRight(base.Path, "/") + urlPath
	u := &url.URL{Scheme: base.Scheme, Host: base.Host, Path: fullPath, RawQuery: rawQuery}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	region := cfg.S3Region
	if region == "" {
		region = "us-east-1"
	}

	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Host", base.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonURI := u.EscapedPath()
	if canonURI == "" {
		canonURI = "/"
	}
	canonHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", strings.ToLower(base.Host), payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonReq := strings.Join([]string{method, canonURI, rawQuery, canonHeaders, signedHeaders, payloadHash}, "\n")
	canonHash := sha256.Sum256([]byte(canonReq))

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	strToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(canonHash[:])}, "\n")
	sigKey := deriveSigV4Key(cfg.S3SecretKey, dateStamp, region, "s3")
	sig := hex.EncodeToString(hmacSHA256(sigKey, []byte(strToSign)))

	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		cfg.S3AccessKey, scope, signedHeaders, sig))

	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if respOut != nil {
		*respOut = rb
	}
	return nil
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func deriveSigV4Key(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// ---- WebDAV client (PUT + PROPFIND + DELETE) --------------------------------

func uploadAndPruneWebDAV(ctx context.Context, cfg remoteBackupConfig, filePath, fileName string) (int, error) {
	if cfg.WebDAVURL == "" {
		return 0, errors.New("WebDAV URL is required.")
	}
	if err := checkHTTPURL(cfg.WebDAVURL, true); err != nil {
		return 0, fmt.Errorf("WebDAV URL: %w", err)
	}
	payload, err := os.ReadFile(filePath)
	if err != nil {
		return 0, err
	}
	c := guardedClient(3*time.Minute, cfg.WebDAVAllowPrivate)
	defer c.CloseIdleConnections()

	target := strings.TrimRight(cfg.WebDAVURL, "/") + "/" + fileName
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	putReq.ContentLength = int64(len(payload))
	if cfg.WebDAVUsername != "" || cfg.WebDAVPassword != "" {
		putReq.SetBasicAuth(cfg.WebDAVUsername, cfg.WebDAVPassword)
	}
	resp, err := c.Do(putReq)
	if err != nil {
		return 0, fmt.Errorf("WebDAV upload: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("WebDAV upload HTTP %d", resp.StatusCode)
	}

	if cfg.Retention <= 0 {
		return 0, nil
	}
	propReq, err := http.NewRequestWithContext(ctx, "PROPFIND", strings.TrimRight(cfg.WebDAVURL, "/")+"/", nil)
	if err != nil {
		return 0, nil
	}
	propReq.Header.Set("Depth", "1")
	if cfg.WebDAVUsername != "" || cfg.WebDAVPassword != "" {
		propReq.SetBasicAuth(cfg.WebDAVUsername, cfg.WebDAVPassword)
	}
	presp, err := c.Do(propReq)
	if err != nil {
		return 0, nil
	}
	pbody, _ := io.ReadAll(io.LimitReader(presp.Body, 2<<20))
	presp.Body.Close()
	if presp.StatusCode < 200 || presp.StatusCode >= 300 {
		return 0, nil
	}
	var multi struct {
		Responses []struct {
			Href string `xml:"href"`
		} `xml:"response"`
	}
	if xml.Unmarshal(pbody, &multi) != nil {
		return 0, nil
	}
	var names []string
	for _, r := range multi.Responses {
		base := path.Base(strings.TrimRight(r.Href, "/"))
		if strings.HasPrefix(base, "moorage-backup-") && strings.HasSuffix(base, ".mbk") {
			names = append(names, base)
		}
	}
	sort.Strings(names)
	pruned := 0
	for len(names) > cfg.Retention {
		old := names[0]
		names = names[1:]
		delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(cfg.WebDAVURL, "/")+"/"+old, nil)
		if err != nil {
			continue
		}
		if cfg.WebDAVUsername != "" || cfg.WebDAVPassword != "" {
			delReq.SetBasicAuth(cfg.WebDAVUsername, cfg.WebDAVPassword)
		}
		if dresp, err := c.Do(delReq); err == nil {
			dresp.Body.Close()
			if dresp.StatusCode >= 200 && dresp.StatusCode < 300 {
				pruned++
			}
		}
	}
	return pruned, nil
}
