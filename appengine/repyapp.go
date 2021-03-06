package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/errorreporting"
	"cloud.google.com/go/storage"
	"github.com/lutzky/repy"
	"github.com/lutzky/repy/recode"
	"github.com/lutzky/repy/writerlogger"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"golang.org/x/text/encoding/charmap"
	"google.golang.org/api/iterator"
)

var bucketName string

const defaultBucketName = "staging.repy-176217.appspot.com"

func downloadREPYZip(ctx context.Context) ([]byte, error) {
	resp, err := http.Get(repy.RepFileURL)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to download %q", repy.RepFileURL)
	}
	defer resp.Body.Close()

	return repy.ExtractFromZip(resp.Body)
}

var errorClient *errorreporting.Client

func initErrorReporting(ctx context.Context) {
	gcpProject := os.Getenv("GCP_PROJECT")
	log.Printf("GCP_PROJECT: %q", gcpProject)

	if gcpProject == "" {
		log.Println("Skipping error reporting, as GCP_PROJECT is unset")
		return
	}

	var err error

	errorClient, err = errorreporting.NewClient(ctx, gcpProject, errorreporting.Config{
		ServiceName: "repyapp",
		OnError: func(err error) {
			log.Printf("Could not log error: %v", err)
		},
	})
	if err != nil {
		log.Fatalf("Failed to create error reporting client: %v", err)
	}
}

func main() {
	http.HandleFunc("/update", handler)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
		log.Printf("Defaulting to port %s", port)
	}

	initErrorReporting(context.Background())

	bucketName = os.Getenv("CLOUD_STORAGE_BUCKET")
	if bucketName == "" {
		bucketName = defaultBucketName
		log.Printf("Using default cloud storage bucket")
	}
	log.Printf("Using cloud storage bucket %q", bucketName)

	log.Printf("Listening on port %s", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", port), nil))
}

type repyStorer struct {
	ctx           context.Context
	bucket        *storage.BucketHandle
	data          []byte
	sha1sum       [20]byte
	cacheDisabled bool
}

func newRepyStorer(ctx context.Context, data []byte) (*repyStorer, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get storage client")
	}

	result := &repyStorer{
		ctx:     ctx,
		bucket:  client.Bucket(bucketName),
		data:    data,
		sha1sum: sha1.Sum(data),
	}

	log.Printf("REPY SHA1SUM: %x", result.sha1sum)

	return result, nil
}

func (rs *repyStorer) writeAllREPYFiles() error {
	repyBytesISO8859_8, err := recode.Recode(charmap.CodePage862, charmap.ISO8859_8, rs.data)
	if err != nil {
		return errors.Wrap(err, "failed to convert REPY to ISO8859-8")
	}

	baseFileName := fmt.Sprintf("%x.repy", rs.sha1sum)

	isMissing := true

	if exists, err := rs.fileExists(baseFileName); err != nil {
		return errors.Wrapf(err, "Coudln't check if %q already exists", baseFileName)
	} else if exists {
		log.Printf("%q already exists", baseFileName)
		isMissing = false
	}

	destinations := []struct {
		filename      string
		contentType   string
		data          []byte
		onlyIfMissing bool
	}{
		{baseFileName, "text/plain; charset=cp862", rs.data, true},
		{fmt.Sprintf("%x.txt", rs.sha1sum), "text/plain; charset=iso8859-8", repyBytesISO8859_8, true},
		{"latest.txt", "text/plain; charset=iso8859-8", repyBytesISO8859_8, false},
		{"latest.repy", "text/plain; charset=cp862", rs.data, false},
	}

	var g errgroup.Group
	for _, dest := range destinations {
		dest := dest
		g.Go(func() error {
			if dest.onlyIfMissing && !isMissing {
				return nil
			}
			log.Printf("writing %q with content-type %q", dest.filename, dest.contentType)
			if err := rs.copyToFile(dest.filename, bytes.NewReader(dest.data)); err != nil {
				return errors.Wrapf(err, "failed to write %q", dest.filename)
			}
			if err := rs.setContentType(dest.filename, dest.contentType); err != nil {
				return err
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	if isMissing {
		f := fmt.Sprintf("%x.timestamp", rs.sha1sum)
		log.Printf("Writing timestamp file %q", f)
		if err := rs.writeTimeStamp(f, time.Now()); err != nil {
			return err
		}
	}

	return nil
}

func (rs *repyStorer) parseJSONAndWrite() error {
	parseLogWriter, parseLogCloser := rs.makePublicObject("latest.parse.log")
	defer parseLogCloser()

	catalog, err := repy.ReadFile(bytes.NewReader(rs.data), writerlogger.Logger{W: parseLogWriter})
	if err != nil {
		fmt.Fprintf(parseLogWriter, "Read returned error: %v\n", err)
		return errors.Wrap(err, "failed to read catalog")
	}

	for _, filename := range []string{fmt.Sprintf("%x.json", rs.sha1sum), "latest.json"} {
		jsonWriter, jsonCloser := rs.makePublicObject(filename)
		defer jsonCloser()
		enc := json.NewEncoder(jsonWriter)
		enc.SetIndent("", "  ")

		if err := enc.Encode(catalog); err != nil {
			return errors.Wrapf(err, "failed to write %q", filename)
		}
	}

	return nil
}

const (
	catalogFileName = "catalog.json"
	cronHeader      = "X-Appengine-Cron"
)

func handler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Header.Get(cronHeader) == "" {
		log.Printf("Missing header %q", cronHeader)
		httpErrorWrap(ctx, w, r, fmt.Errorf("Access denied"), "Access denied")
		return
	}

	repyBytes, err := downloadREPYZip(ctx)
	if err != nil {
		httpErrorWrap(ctx, w, r, err, "Failed to download REPY zip")
		return
	}

	rs, err := newRepyStorer(ctx, repyBytes)
	if err != nil {
		httpErrorWrap(ctx, w, r, err, "Failed to initialize REPY App")
		return
	}

	if r.FormValue("nocache") == "1" {
		rs.cacheDisabled = true
	}

	if err := rs.writeAllREPYFiles(); err != nil {
		httpErrorWrap(ctx, w, r, err, "Failed to write REPY files")
		return
	}

	if err := rs.parseJSONAndWrite(); err != nil {
		httpErrorWrap(ctx, w, r, err, "Failed to complete JSON Parsing")
		return
	}

	log.Printf("Successfully parsed REPY")

	log.Printf("Writing %s", catalogFileName)

	if err := rs.writeCatalog(); err != nil {
		httpErrorWrap(ctx, w, r, err, "Failed to write catalog")
		return
	}

	log.Printf("Success")
	fmt.Fprintf(w, "Success")
}

func (rs *repyStorer) copyToFile(filename string, r io.Reader) error {
	w, closer := rs.makePublicObject(filename)
	defer closer()

	_, err := io.Copy(w, r)

	return err
}

func httpErrorWrap(ctx context.Context, w http.ResponseWriter, r *http.Request, err error, msg string) {
	log.Printf(errors.Wrap(err, msg).Error())
	if errorClient != nil {
		errorClient.Report(errorreporting.Entry{
			Error: err,
			Req:   r,
		})
	}
	http.Error(w, msg, http.StatusInternalServerError)
}

// makePublicObject opens a file in the specified client and bucket with the
// given name, and returns a writer to it as well as a closer function. Caller
// must call the closer function when done writing to the file (e.g. using
// defer). The object will be made public upon closing.
func (rs *repyStorer) makePublicObject(filename string) (io.Writer, func()) {
	obj := rs.bucket.Object(filename)
	w := obj.NewWriter(rs.ctx)
	closer := func() {
		w.Close()
		if err := obj.ACL().Set(rs.ctx, storage.AllUsers, storage.RoleReader); err != nil {
			log.Printf("Failed to make %q public: %v", filename, err)
		}
	}
	return w, closer
}

func (rs *repyStorer) setContentType(filename string, contentType string) error {
	obj := rs.bucket.Object(filename)
	if _, err := obj.Update(rs.ctx, storage.ObjectAttrsToUpdate{ContentType: contentType}); err != nil {
		return errors.Wrapf(err, "failed to set content-type for %q to %q", filename, contentType)
	}

	return nil
}

func (rs *repyStorer) fileExists(filename string) (bool, error) {
	obj := rs.bucket.Object(filename)
	_, err := obj.Attrs(rs.ctx)
	switch err {
	case nil:
		return true, nil
	case storage.ErrObjectNotExist:
		return false, nil
	default:
		return false, errors.Wrapf(err, "couldn't check if %q exists", filename)
	}
}

func (rs *repyStorer) writeTimeStamp(filename string, t time.Time) error {
	w, closer := rs.makePublicObject(filename)
	defer closer()
	if _, err := fmt.Fprintf(w, "%s\n", t.UTC().Format(time.UnixDate)); err != nil {
		return errors.Wrapf(err, "couldn't write timestamp %q", filename)
	}
	return nil
}

type metadataEntry struct {
	sha1sum  string
	t        time.Time
	semester string
}

// Catalog is the format for catalogFileName
type Catalog struct {
	Entries []CatalogEntry
}

// CatalogEntry is an entry for Catalog
type CatalogEntry struct {
	Sha1Sum   string
	Original  string
	Iso8859_8 string
	Parsed    string
	TimeStamp time.Time
	Semester  string
}

func (rs *repyStorer) writeCatalog() error {
	metadata, err := rs.getAllMetadata()
	if err != nil {
		return errors.Wrap(err, "failed to get timestamps and sums")
	}

	catalog := Catalog{Entries: []CatalogEntry{}}

	for _, m := range metadata {
		catalog.Entries = append(catalog.Entries, CatalogEntry{
			Sha1Sum:   m.sha1sum,
			TimeStamp: m.t,
			Semester:  m.semester,
			Iso8859_8: m.sha1sum + ".txt",
			Original:  m.sha1sum + ".repy",
			Parsed:    m.sha1sum + ".json",
		})
	}

	jsonBytes, err := json.Marshal(catalog)
	if err != nil {
		return errors.Wrap(err, "failed to format JSON")
	}

	if err := rs.copyToFile(catalogFileName, bytes.NewReader(jsonBytes)); err != nil {
		return errors.Wrapf(err, "failed to write JSON to %q", catalogFileName)
	}

	return nil
}

func (rs *repyStorer) loadMetaCatalog() map[string]metadataEntry {
	if rs.cacheDisabled {
		log.Printf("Bypassing cache")
		return nil
	}

	log.Printf("Reading cached catalog from %q", catalogFileName)

	obj := rs.bucket.Object(catalogFileName)
	r, err := obj.NewReader(rs.ctx)
	if err != nil {
		log.Printf("Failed to read %q: %v", catalogFileName, err)
		return nil
	}
	dec := json.NewDecoder(r)
	catalog := Catalog{}

	if err := dec.Decode(&catalog); err != nil {
		log.Printf("Failed to unmarshal %q: %v", catalogFileName, err)
	}

	result := map[string]metadataEntry{}
	for _, entry := range catalog.Entries {
		result[entry.Sha1Sum] = metadataEntry{
			sha1sum:  entry.Sha1Sum,
			semester: entry.Semester,
			t:        entry.TimeStamp,
		}
	}
	return result
}

func (rs *repyStorer) getAllMetadata() ([]metadataEntry, error) {
	sums, err := rs.getExistingSHA1Sums()
	if err != nil {
		return nil, errors.Wrap(err, "failed to list existing REPY data")
	}

	cache := rs.loadMetaCatalog()

	var wg errgroup.Group
	ch := make(chan metadataEntry)

	for _, sum := range sums {
		sum := sum
		wg.Go(func() error {
			if entry, ok := cache[sum]; ok {
				ch <- entry
				return nil
			}

			ts, err := rs.getTimeStampForSHA1Sum(sum)
			if err != nil {
				log.Printf("Couldn't get timestamp for %q: %v", sum, err)
				return err
			}

			semester := rs.getSemesterForSHA1Sum(sum)

			ch <- metadataEntry{
				sha1sum:  sum,
				t:        ts,
				semester: semester,
			}
			return nil
		})
	}

	results := []metadataEntry{}

	var wg2 sync.WaitGroup

	wg2.Add(1)
	go func() {
		for ts := range ch {
			results = append(results, ts)
		}
		wg2.Done()
	}()

	err = wg.Wait()
	close(ch)

	wg2.Wait()

	return results, err
}

var repyFileRegexp = regexp.MustCompile(`[0-9a-f]{20}.repy`)

func (rs *repyStorer) getExistingSHA1Sums() ([]string, error) {
	result := []string{}

	it := rs.bucket.Objects(rs.ctx, nil)
	for {
		objAttrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, "Problem iterating")
		}

		if repyFileRegexp.MatchString(objAttrs.Name) {
			sha1sum := strings.TrimSuffix(objAttrs.Name, ".repy")
			result = append(result, sha1sum)
		}
	}

	return result, nil
}

func (rs *repyStorer) getTimeStampForSHA1Sum(sha1sum string) (time.Time, error) {
	filename := sha1sum + ".timestamp"
	obj := rs.bucket.Object(filename)
	r, err := obj.NewReader(rs.ctx)
	if err != nil {
		return time.Time{}, errors.Wrapf(err, "couldn't open %q", filename)
	}

	data, err := ioutil.ReadAll(r)
	if err != nil {
		return time.Time{}, errors.Wrapf(err, "couldn't read from %q", filename)
	}

	timestamp := strings.TrimSpace(string(data))

	t, err := time.Parse(time.UnixDate, timestamp)
	if err != nil {
		return time.Time{}, errors.Wrapf(err, "couldn't parse time %q", timestamp)
	}

	return t, nil
}

func (rs *repyStorer) getSemesterForSHA1Sum(sha1sum string) string {
	filename := sha1sum + ".json"
	obj := rs.bucket.Object(filename)
	r, err := obj.NewReader(rs.ctx)
	if err != nil {
		log.Printf("Failed to read %q: %v", filename, err)
		return ""
	}

	dec := json.NewDecoder(r)
	catalog := repy.Catalog{}
	if err := dec.Decode(&catalog); err != nil {
		log.Printf("Failed to unmarshal %q: %v", filename, err)
		return ""
	}

	if len(catalog) == 0 {
		return "No faculties"
	}

	semester := catalog[0].Semester

	for _, faculty := range catalog {
		if faculty.Semester != semester {
			return "INCONSISTENT"
		}
	}

	return semester
}
