// Package utils contains common helper functions used across multiple other packages.
// Utils should not import any harbourbridge packages.
package utils

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	sp "cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/storage"
	"github.com/cloudspannerecosystem/harbourbridge/common/constants"
	"github.com/cloudspannerecosystem/harbourbridge/internal"
	"github.com/cloudspannerecosystem/harbourbridge/sources/common"
	"github.com/cloudspannerecosystem/harbourbridge/sources/spanner"
	"golang.org/x/crypto/ssh/terminal"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	instancepb "google.golang.org/genproto/googleapis/spanner/admin/instance/v1"
)

// IOStreams is a struct that contains the file descriptor for dumpFile.
type IOStreams struct {
	In, SeekableIn, Out *os.File
	BytesRead           int64
}

// Harbourbridge accepts a manifest file in the form of a json which unmarshalls into the ManifestTables struct.
type ManifestTable struct {
	Table_name    string   `json:"table_name"`
	File_patterns []string `json:"file_patterns"`
}

// NewIOStreams returns a new IOStreams struct such that input stream is set
// to open file descriptor for dumpFile if driver is PGDUMP or MYSQLDUMP.
// Input stream defaults to stdin. Output stream is always set to stdout.
func NewIOStreams(driver string, dumpFile string) IOStreams {
	io := IOStreams{In: os.Stdin, Out: os.Stdout}
	u, err := url.Parse(dumpFile)
	if err != nil {
		fmt.Printf("parseFilePath: unable parse file path for dumpfile %s", dumpFile)
		log.Fatal(err)
	}
	if (driver == constants.PGDUMP || driver == constants.MYSQLDUMP) && dumpFile != "" {
		fmt.Printf("\nLoading dump file from path: %s\n", dumpFile)
		var f *os.File
		var err error
		if u.Scheme == "gs" {
			bucketName := u.Host
			filePath := u.Path[1:] // removes "/" from beginning of path
			f, err = DownloadFromGCS(bucketName, filePath, "harbourbridge.gcs.data")
		} else {
			f, err = os.Open(dumpFile)
		}
		if err != nil {
			fmt.Printf("\nError reading dump file: %v err:%v\n", dumpFile, err)
			log.Fatal(err)
		}
		io.In = f
	}
	return io
}

// DownloadFromGCS returns the dump file that is downloaded from GCS.
func DownloadFromGCS(bucketName, filePath, tmpFile string) (*os.File, error) {
	ctx := context.Background()

	client, err := storage.NewClient(ctx)
	if err != nil {
		fmt.Printf("Failed to create GCS client for bucket %q", bucketName)
		log.Fatal(err)
	}
	defer client.Close()

	bucket := client.Bucket(bucketName)
	rc, err := bucket.Object(filePath).NewReader(ctx)
	if err != nil {
		fmt.Printf("readFile: unable to open file from bucket %q, file %q: %v", bucketName, filePath, err)
		log.Fatal(err)
		return nil, err
	}
	defer rc.Close()
	r := bufio.NewReader(rc)

	tmpDir := os.TempDir() + constants.HB_TMP_DIR
	os.MkdirAll(tmpDir, os.ModePerm)
	tmpfile, err := os.Create(tmpDir + "/" + tmpFile)
	if err != nil {
		fmt.Printf("saveFile: unable to open temporary file to save dump file from GCS bucket %v", err)
		log.Fatal(err)
		return nil, err
	}

	fmt.Printf("\nDownloading file from GCS bucket %s, path %s\n", bucketName, filePath)
	buffer := make([]byte, 1024)
	for {
		// read a chunk
		n, err := r.Read(buffer[:cap(buffer)])

		if err != nil && err != io.EOF {
			fmt.Printf("readFile: unable to read entire file from bucket %s, file %s: %v", bucketName, filePath, err)
			log.Fatal(err)
			return nil, err
		}
		if n == 0 && err == io.EOF {
			break
		}

		// write a chunk
		if _, err = tmpfile.Write(buffer[:n]); err != nil {
			fmt.Printf("saveFile: unable to save read data from bucket %s, file %s: %v", bucketName, filePath, err)
			log.Fatal(err)
		}
	}

	return tmpfile, nil
}

// PreloadGCSFiles downloads gcs files to tmp and updates the file paths in manifest with the local path.
func PreloadGCSFiles(tables []ManifestTable) ([]ManifestTable, error) {
	for i, table := range tables {
		for j, filePath := range table.File_patterns {
			u, err := url.Parse(filePath)
			if err != nil {
				return nil, fmt.Errorf("unable parse file path %s for table %s", filePath, table.Table_name)
			}
			if u.Scheme == "gs" {
				bucketName := u.Host
				filePath := u.Path[1:] // removes "/" from beginning of path
				tmpFile := strings.ReplaceAll(filePath, "/", ".")
				// Files get downloaded to tmp dir.
				fileLoc := os.TempDir() + constants.HB_TMP_DIR + "/" + tmpFile
				_, err = DownloadFromGCS(bucketName, filePath, tmpFile)
				if err != nil {
					return nil, fmt.Errorf("cannot download gcs file: %s for table %s", filePath, table.Table_name)
				}
				tables[i].File_patterns[j] = fileLoc
				fmt.Printf("Downloaded file: %s\n", fileLoc)
			}
		}
	}
	return tables, nil
}

// GetProject returns the cloud project we should use for accessing Spanner.
// Use environment variable GCLOUD_PROJECT if it is set.
// Otherwise, use the default project returned from gcloud.
func GetProject() (string, error) {
	project := os.Getenv("GCLOUD_PROJECT")
	if project != "" {
		return project, nil
	}
	cmd := exec.Command("gcloud", "config", "list", "--format", "value(core.project)")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("call to gcloud to get project failed: %w", err)
	}
	project = strings.TrimSpace(string(out))
	return project, nil
}

// GetInstance returns the Spanner instance we should use for creating DBs.
// If the user specified instance (via flag 'instance') then use that.
// Otherwise try to deduce the instance using gcloud.
func GetInstance(ctx context.Context, project string, out *os.File) (string, error) {
	l, err := getInstances(ctx, project)
	if err != nil {
		return "", err
	}
	if len(l) == 0 {
		fmt.Fprintf(out, "Could not find any Spanner instances for project %s\n", project)
		return "", fmt.Errorf("no Spanner instances for %s", project)
	}

	// Note: we could ask for user input to select/confirm which Spanner
	// instance to use, but that interacts poorly with piping pg_dump/mysqldump data
	// to the tool via stdin.
	if len(l) == 1 {
		fmt.Fprintf(out, "Using only available Spanner instance: %s\n", l[0])
		return l[0], nil
	}
	fmt.Fprintf(out, "Available Spanner instances:\n")
	for i, x := range l {
		fmt.Fprintf(out, " %d) %s\n", i+1, x)
	}
	fmt.Fprintf(out, "Please pick one of the available instances and set the flag '--instance'\n\n")
	return "", fmt.Errorf("auto-selection of instance failed: project %s has more than one Spanner instance. "+
		"Please use the flag '--instance' to select an instance", project)
}

func getInstances(ctx context.Context, project string) ([]string, error) {
	instanceClient, err := instance.NewInstanceAdminClient(ctx)
	if err != nil {
		return nil, AnalyzeError(err, fmt.Sprintf("projects/%s", project))
	}
	it := instanceClient.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: fmt.Sprintf("projects/%s", project)})
	var l []string
	for {
		resp, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, AnalyzeError(err, fmt.Sprintf("projects/%s", project))
		}
		l = append(l, strings.TrimPrefix(resp.Name, fmt.Sprintf("projects/%s/instances/", project)))
	}
	return l, nil
}

func GetPassword() string {
	fmt.Print("Enter Password: ")
	bytePassword, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		fmt.Println("\nCoudln't read password")
		return ""
	}
	fmt.Printf("\n")
	return strings.TrimSpace(string(bytePassword))
}

// GetDatabaseName generates database name with driver_date prefix.
func GetDatabaseName(driver string, now time.Time) (string, error) {
	return generateName(fmt.Sprintf("%s_%s", driver, now.Format("2006-01-02")))
}

func generateName(prefix string) (string, error) {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	if err != nil {
		return "", fmt.Errorf("error generating name: %w", err)

	}
	return fmt.Sprintf("%s_%x-%x", prefix, b[0:2], b[2:4]), nil
}

// parseURI parses an unknown URI string that could be a database, instance or project URI.
func parseURI(URI string) (project, instance, dbName string) {
	project, instance, dbName = "", "", ""
	if strings.Contains(URI, "databases") {
		project, instance, dbName = ParseDbURI(URI)
	} else if strings.Contains(URI, "instances") {
		project, instance = parseInstanceURI(URI)
	} else if strings.Contains(URI, "projects") {
		project = parseProjectURI(URI)
	}
	return
}

func ParseDbURI(dbURI string) (project, instance, dbName string) {
	split := strings.Split(dbURI, "/databases/")
	project, instance = parseInstanceURI(split[0])
	dbName = split[1]
	return
}

func parseInstanceURI(instanceURI string) (project, instance string) {
	split := strings.Split(instanceURI, "/instances/")
	project = parseProjectURI(split[0])
	instance = split[1]
	return
}

func parseProjectURI(projectURI string) (project string) {
	split := strings.Split(projectURI, "/")
	project = split[1]
	return
}

// AnalyzeError inspects an error returned from Cloud Spanner and adds information
// about potential root causes e.g. authentication issues.
func AnalyzeError(err error, URI string) error {
	project, instance, _ := parseURI(URI)
	e := strings.ToLower(err.Error())
	if ContainsAny(e, []string{"unauthenticated", "cannot fetch token", "default credentials"}) {
		return fmt.Errorf("%w."+`
Possible cause: credentials are mis-configured. Do you need to run

  gcloud auth application-default login

or configure environment variable GOOGLE_APPLICATION_CREDENTIALS.
See https://cloud.google.com/docs/authentication/getting-started`, err)
	}
	if ContainsAny(e, []string{"instance not found"}) && instance != "" {
		return fmt.Errorf("%w.\n"+`
Possible cause: Spanner instance specified via instance option does not exist.
Please check that '%s' is correct and that it is a valid Spanner
instance for project %s`, err, instance, project)
	}
	return err
}

// PrintPermissionsWarning prints permission warning.
func PrintPermissionsWarning(driver string, out *os.File) {
	fmt.Fprintf(out,
		`
WARNING: Please check that permissions for this Spanner instance are
appropriate. Spanner manages access control at the database level, and the
database created by HarbourBridge will inherit default permissions from this
instance. All data written to Spanner will be visible to anyone who can
access the created database. Note that `+driver+` table-level and row-level
ACLs are dropped during conversion since they are not supported by Spanner.

`)
}

func ContainsAny(s string, l []string) bool {
	for _, a := range l {
		if strings.Contains(s, a) {
			return true
		}
	}
	return false
}

// CheckEqualSets checks if the set of values in a and b are equal.
func CheckEqualSets(a, b []string) bool {
	tmp_a := append(make([]string, len(a)), a...)
	tmp_b := append(make([]string, len(b)), b...)
	sort.Strings(tmp_a)
	sort.Strings(tmp_b)
	return reflect.DeepEqual(tmp_a, tmp_b)
}

func GetFileSize(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("can't stat file: %w", err)
	}
	return info.Size(), nil
}

// SetupLogFile configures the file used for logs.
// By default we just drop logs on the floor. To enable them (e.g. to debug
// Cloud Spanner client library issues), set logfile to a non-empty filename.
// Note: this tool itself doesn't generate logs, but some of the libraries it
// uses do. If we don't set the log file, we see a number of unhelpful and
// unactionable logs spamming stdout, which is annoying and confusing.
func SetupLogFile() (*os.File, error) {
	// To enable debug logs, set logfile to a non-empty filename.
	logfile := ""
	if logfile == "" {
		log.SetOutput(ioutil.Discard)
		return nil, nil
	}
	f, err := os.Create(logfile)
	if err != nil {
		return nil, err
	}
	log.SetOutput(f)
	return f, nil
}

// Close closes file.
func Close(f *os.File) {
	if f != nil {
		f.Close()
	}
}

func PrintSeekError(driver string, err error, out *os.File) {
	fmt.Fprintf(out, "\nCan't get seekable input file: %v\n", err)
	fmt.Fprintf(out, "Likely cause: not enough space in %s.\n", os.TempDir())
	fmt.Fprintf(out, "Try writing "+driver+" output to a file first i.e.\n")
	fmt.Fprintf(out, " "+driver+" > tmpfile\n")
	fmt.Fprintf(out, "  harbourbridge < tmpfile\n")
}

// NewSpannerClient returns a new Spanner client.
// It respects SPANNER_API_ENDPOINT.
func NewSpannerClient(ctx context.Context, db string) (*sp.Client, error) {
	if endpoint := os.Getenv("SPANNER_API_ENDPOINT"); endpoint != "" {
		return sp.NewClient(ctx, db, option.WithEndpoint(endpoint))
	}
	return sp.NewClient(ctx, db)
}

// GetClient returns a new Spanner client.  It uses the background context.
func GetClient(ctx context.Context, db string) (*sp.Client, error) {
	return NewSpannerClient(ctx, db)
}

// NewDatabaseAdminClient returns a new db-admin client.
// It respects SPANNER_API_ENDPOINT.
func NewDatabaseAdminClient(ctx context.Context) (*database.DatabaseAdminClient, error) {
	if endpoint := os.Getenv("SPANNER_API_ENDPOINT"); endpoint != "" {
		return database.NewDatabaseAdminClient(ctx, option.WithEndpoint(endpoint))
	}
	return database.NewDatabaseAdminClient(ctx)
}

// NewInstanceAdminClient returns a new instance-admin client.
// It respects SPANNER_API_ENDPOINT.
func NewInstanceAdminClient(ctx context.Context) (*instance.InstanceAdminClient, error) {
	if endpoint := os.Getenv("SPANNER_API_ENDPOINT"); endpoint != "" {
		return instance.NewInstanceAdminClient(ctx, option.WithEndpoint(endpoint))
	}
	return instance.NewInstanceAdminClient(ctx)
}

func SumMapValues(m map[string]int64) int64 {
	n := int64(0)
	for _, c := range m {
		n += c
	}
	return n
}

// GetBanner prints banner message after command line process is finished.
func GetBanner(now time.Time, db string) string {
	return fmt.Sprintf("Generated at %s for db %s\n\n", now.Format("2006-01-02 15:04:05"), db)
}

func IsValidDriver(driver string) bool {
	d := strings.ToLower(driver)
	for _, vd := range GetValidDrivers() {
		if d == vd {
			return true
		}
	}
	return false
}

func GetValidDrivers() []string {
	//First 5 drivers support legacy mode. Rest dont.
	return []string{
		constants.POSTGRES,
		constants.PGDUMP,
		constants.MYSQL,
		constants.MYSQLDUMP,
		constants.DYNAMODB,

		constants.SQLSERVER,
	}
}

func IsLegacyModeSupportedDriver(driver string) bool {
	d := strings.ToLower(driver)
	lds := GetLegacyModeSupportedDrivers()
	for _, ld := range lds {
		if d == ld {
			return true
		}
	}
	return false
}

func GetLegacyModeSupportedDrivers() []string {
	return GetValidDrivers()[:5]
}

// ReadSpannerSchema fills conv by querying Spanner infoschema treating Spanner as both the source and dest.
func ReadSpannerSchema(ctx context.Context, conv *internal.Conv, client *sp.Client) error {
	infoSchema := spanner.InfoSchemaImpl{Client: client, Ctx: ctx, TargetDb: conv.TargetDb}
	err := common.ProcessSchema(conv, infoSchema)
	if err != nil {
		return fmt.Errorf("error trying to read and convert spanner schema: %v", err)
	}
	parentTables, err := infoSchema.GetInterleaveTables()
	if err != nil {
		// We should ideally throw an error here as it could potentially cause a lot of failed writes.
		// We raise an unexpected error for now to make it compatible with the integration tests.
		// In the emulator, the interleave_type column in not supported hence the query fails.
		conv.Unexpected(fmt.Sprintf("error trying to fetch interleave table info from schema: %v", err))
	}
	// Assign parents if any.
	for table, parent := range parentTables {
		spTable := conv.SpSchema[table]
		spTable.Parent = parent
		conv.SpSchema[table] = spTable
	}
	return nil
}

func DialectToTarget(dialect string) string {
	if strings.ToLower(dialect) == constants.DIALECT_POSTGRESQL {
		return constants.TargetExperimentalPostgres
	}
	return constants.TargetSpanner
}