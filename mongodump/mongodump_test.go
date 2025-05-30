// copyright (C) MongoDB, Inc. 2014-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package mongodump

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mongodb/mongo-tools/common/archive"
	"github.com/mongodb/mongo-tools/common/bsonutil"
	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/failpoint"
	"github.com/mongodb/mongo-tools/common/json"
	"github.com/mongodb/mongo-tools/common/log"
	"github.com/mongodb/mongo-tools/common/options"
	"github.com/mongodb/mongo-tools/common/testtype"
	"github.com/mongodb/mongo-tools/common/testutil"
	"github.com/mongodb/mongo-tools/common/util"
	"github.com/pkg/errors"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

var (
	// database with test data.
	testDB = "mongodump_test_db"
	// temp database used for restoring a DB.
	testRestoreDB       = "temp_mongodump_restore_test_db"
	testCollectionNames = []string{"coll1", "coll2", "coll/three"}
)

const (
	longPrefix = "aVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVery" +
		"VeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVery" +
		"VeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVery"
	longCollectionName = longPrefix + "/Long/Collection/Name/Consisting/Of/Many/Characters"
	longBsonName       = longPrefix + "%2FLong%2FCollection%2FName%24FUVlwTrb2eHN1RUE1swI1fFzWmA.bson"
	longMetadataName   = longPrefix + "%2FLong%2FCollection%2FName%24FUVlwTrb2eHN1RUE1swI1fFzWmA.metadata.json"
)

func simpleMongoDumpInstance() *MongoDump {
	var toolOptions *options.ToolOptions

	// get ToolOptions from URI or defaults
	if uri := os.Getenv("TOOLS_TESTING_MONGOD"); uri != "" {
		fakeArgs := []string{"--uri=" + uri}
		toolOptions = options.New("mongodump", "", "", "", true, options.EnabledOptions{URI: true})
		_, err := toolOptions.ParseArgs(fakeArgs)
		if err != nil {
			panic("Could not parse MONGOD environment variable")
		}
	} else {
		ssl := testutil.GetSSLOptions()
		auth := testutil.GetAuthOptions()
		connection := &options.Connection{
			Host: "localhost",
			Port: db.DefaultTestPort,
		}
		toolOptions = &options.ToolOptions{
			SSL:        &ssl,
			Connection: connection,
			Auth:       &auth,
			Verbosity:  &options.Verbosity{},
			URI:        &options.URI{},
		}
	}

	// Limit ToolOptions to test database
	toolOptions.Namespace = &options.Namespace{DB: testDB}

	outputOptions := &OutputOptions{
		NumParallelCollections: 1,
	}
	inputOptions := &InputOptions{}

	log.SetVerbosity(toolOptions.Verbosity)

	return &MongoDump{
		ToolOptions:   toolOptions,
		InputOptions:  inputOptions,
		OutputOptions: outputOptions,
	}
}

// returns the number of .bson files in a directory
// excluding system.indexes.bson.
func countNonIndexBSONFiles(dir string) (int, error) {
	files, err := listNonIndexBSONFiles(dir)
	if err != nil {
		return 0, err
	}
	return len(files), nil
}

func assertBSONEqual(t *testing.T, expected, actual any) {
	expectedJSON, err := bson.MarshalExtJSONIndent(expected, false, false, "", "    ")
	require.NoError(t, err)

	actualJSON, err := bson.MarshalExtJSONIndent(actual, false, false, "", "    ")
	require.NoError(t, err)

	assert.Equal(t, string(expectedJSON), string(actualJSON))
}

func listNonIndexBSONFiles(dir string) ([]string, error) {
	var files []string
	matchingFiles, err := getMatchingFiles(dir, ".*\\.bson")
	if err != nil {
		return nil, err
	}
	for _, fileName := range matchingFiles {
		if fileName != "system.indexes.bson" {
			files = append(files, fileName)
		}
	}
	return files, nil
}

// returns count of metadata files.
func countMetaDataFiles(dir string) (int, error) {
	matchingFiles, err := getMatchingFiles(dir, ".*\\.metadata\\.json")
	if err != nil {
		return 0, err
	}
	return len(matchingFiles), nil
}

// returns count of oplog entries with 'ui' field.
func countOplogUI(iter *db.DecodedBSONSource) int {
	var count int
	var doc bson.M
	for iter.Next(&doc) {
		count += countOpsWithUI(doc)
	}
	return count
}

func countOpsWithUI(doc bson.M) int {
	var count int
	switch doc["op"] {
	case "i", "u", "d":
		if _, ok := doc["ui"]; ok {
			count++
		}
	case "c":
		if _, ok := doc["ui"]; ok {
			count++
		} else if v, ok := doc["o"]; ok {
			opts, _ := v.(bson.M)
			if applyOps, ok := opts["applyOps"]; ok {
				//nolint:errcheck
				list := applyOps.([]bson.M)
				for _, v := range list {
					count += countOpsWithUI(v)
				}
			}
		}
	}
	return count
}

// returns filenames that match the given pattern.
func getMatchingFiles(dir, pattern string) ([]string, error) {
	fileInfos, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	matchingFiles := []string{}
	var matched bool
	for _, fileInfo := range fileInfos {
		fileName := fileInfo.Name()
		if matched, err = regexp.MatchString(pattern, fileName); matched {
			matchingFiles = append(matchingFiles, fileName)
		}
		if err != nil {
			return nil, err
		}
	}
	return matchingFiles, nil
}

// read all the database bson documents from dir and put it into another DB
// ignore the indexes for now.
func readBSONIntoDatabase(dir, restoreDBName string) error {
	if ok := fileDirExists(dir); !ok {
		return fmt.Errorf("error finding '%v' on local FS", dir)
	}

	session, err := testutil.GetBareSession()
	if err != nil {
		return err
	}

	fileInfos, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, fileInfo := range fileInfos {
		fileName := fileInfo.Name()
		if !strings.HasSuffix(fileName, ".bson") || fileName == "system.indexes.bson" {
			continue
		}

		collectionName, err := util.UnescapeCollectionName(
			fileName[:strings.LastIndex(fileName, ".bson")],
		)
		if err != nil {
			return err
		}

		collection := session.Database(restoreDBName).Collection(collectionName)

		file, err := os.Open(fmt.Sprintf("%s/%s", dir, fileName))
		if err != nil {
			return err
		}
		defer file.Close()

		bsonSource := db.NewDecodedBSONSource(db.NewBSONSource(file))
		defer bsonSource.Close()

		var result bson.D
		for bsonSource.Next(&result) {
			_, err = collection.InsertOne(context.Background(), result)
			if err != nil {
				return err
			}
		}
		if err = bsonSource.Err(); err != nil {
			return err
		}
	}

	return nil
}

func setUpMongoDumpTestData() error {
	session, err := testutil.GetBareSession()
	if err != nil {
		return err
	}

	for i, collectionName := range testCollectionNames {
		coll := session.Database(testDB).Collection(collectionName)

		for j := 0; j < 10*(i+1); j++ {
			_, err = coll.InsertOne(
				context.Background(),
				bson.M{
					"collectionName": collectionName,
					"age":            j,
					"":               "foo",
					"coords":         bson.D{{"x", i}, {"y", j}},
				},
			)
			if err != nil {
				return err
			}

			idx := mongo.IndexModel{
				Keys: bson.M{`"`: 1},
			}
			_, err = coll.Indexes().CreateOne(context.Background(), idx)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func setUpTimeseries(dbName string, colName string) error {
	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		return err
	}

	timeseriesOptions := bson.D{
		{"timeField", "ts"},
		{"metaField", "my_meta"},
	}
	createCmd := bson.D{
		{"create", colName},
		{"timeseries", timeseriesOptions},
	}
	var r2 bson.D
	err = sessionProvider.Run(createCmd, &r2, dbName)
	if err != nil {
		return err
	}

	coll := sessionProvider.DB(dbName).Collection(colName)

	idx := mongo.IndexModel{
		Keys: bson.D{{"my_meta.device", 1}},
	}
	_, err = coll.Indexes().CreateOne(context.Background(), idx)
	if err != nil {
		return err
	}

	idx = mongo.IndexModel{
		Keys: bson.D{{"ts", 1}, {"my_meta.device", 1}},
	}
	_, err = coll.Indexes().CreateOne(context.Background(), idx)
	if err != nil {
		return err
	}

	for i := 0; i < 1000; i++ {
		metadata := bson.M{
			"device": i % 10,
		}
		_, err = coll.InsertOne(
			context.Background(),
			bson.M{
				"ts":          primitive.NewDateTimeFromTime(time.Now()),
				"my_meta":     metadata,
				"measurement": i,
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func setupTimeseriesWithMixedSchema(dbName string, collName string) error {
	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		return err
	}

	client, err := sessionProvider.GetSession()
	if err != nil {
		return err
	}

	if err := client.Database(dbName).Collection(collName).Drop(context.Background()); err != nil {
		return err
	}

	createCmd := bson.D{
		{"create", collName},
		{"timeseries", bson.D{
			{"timeField", "t"},
			{"metaField", "m"},
		}},
	}

	createRes := sessionProvider.DB(dbName).RunCommand(context.Background(), createCmd)
	if createRes.Err() != nil {
		return createRes.Err()
	}

	// SERVER-84531 was only backported to 7.3.
	// TODO: Run collMod command on 6.0 and 7.0 (TOOLS-3597).
	clientFCV := testutil.GetFCV(client)

	shouldAccommodateMixedSchema := clientFCV == "6.0" || clientFCV == "7.0"
	if !shouldAccommodateMixedSchema {
		cmp, err := testutil.CompareFCV(clientFCV, "7.3")
		if err != nil {
			return errors.Wrapf(err, "failed to compare client FCV (%s)", clientFCV)
		}

		shouldAccommodateMixedSchema = cmp >= 0
	}

	if shouldAccommodateMixedSchema {
		if res := sessionProvider.DB(dbName).RunCommand(context.Background(), bson.D{
			{"collMod", collName},
			{"timeseriesBucketsMayHaveMixedSchemaData", true},
		}); res.Err() != nil {
			return res.Err()
		}
	}

	bucketColl := sessionProvider.DB(dbName).Collection("system.buckets." + collName)
	bucketJSON := `{"_id":{"$oid":"65a6eb806ffc9fa4280ecac4"},"control":{"version":1,"min":{"_id":{"$oid":"65a6eba7e6d2e848e08c3750"},"t":{"$date":"2024-01-16T20:48:00Z"},"a":1},"max":{"_id":{"$oid":"65a6eba7e6d2e848e08c3751"},"t":{"$date":"2024-01-16T20:48:39.448Z"},"a":"a"}},"meta":0,"data":{"_id":{"0":{"$oid":"65a6eba7e6d2e848e08c3750"},"1":{"$oid":"65a6eba7e6d2e848e08c3751"}},"t":{"0":{"$date":"2024-01-16T20:48:39.448Z"},"1":{"$date":"2024-01-16T20:48:39.448Z"}},"a":{"0":"a","1":1}}}`
	var bucketMap map[string]interface{}
	if err := json.Unmarshal([]byte(bucketJSON), &bucketMap); err != nil {
		return err
	}
	if err := bsonutil.ConvertLegacyExtJSONDocumentToBSON(bucketMap); err != nil {
		return err
	}
	if _, err := bucketColl.InsertOne(context.Background(), bucketMap); err != nil {
		return err
	}

	return nil
}

func setUpDBView(dbName string, colName string) error {
	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		return err
	}

	pipeline := []bson.M{{"$project": bson.M{"b": "$a"}}}
	createCmd := bson.D{
		{"create", "test view"},
		{"viewOn", colName},
		{"pipeline", pipeline},
	}
	var r2 bson.D
	err = sessionProvider.Run(createCmd, &r2, dbName)
	if err != nil {
		return err
	}
	return nil
}

func turnOnProfiling(dbName string) error {
	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		return err
	}

	profileCmd := bson.D{
		{"profile", 2},
	}

	var res bson.M
	return sessionProvider.Run(profileCmd, &res, dbName)
}

func countSnapshotCmds(profileCollection *mongo.Collection, ns string) (int64, error) {
	return profileCollection.CountDocuments(context.Background(),
		bson.D{
			{"ns", ns},
			{"op", "query"},
			{"$or", []interface{}{
				// 4.0+
				bson.D{{"command.hint._id", 1}},
				// 3.6
				bson.D{{"command.$snapshot", true}},
				bson.D{{"command.snapshot", true}},
				// 3.4 and previous
				bson.D{{"query.$snapshot", true}},
				bson.D{{"query.snapshot", true}},
				bson.D{{"query.hint._id", 1}},
			}},
		},
	)
}

// backgroundInsert inserts into random collections until provided done
// channel is closed.  The function closes the ready channel to signal that
// background insertion has started.  When the done channel is closed, the
// function returns.  Any errors are passed back on the errs channel.
func backgroundInsert(ready, done chan struct{}, errs chan error) {
	defer close(errs)
	session, err := testutil.GetBareSession()
	if err != nil {
		errs <- err
		close(ready)
		return
	}

	colls := make([]*mongo.Collection, len(testCollectionNames))
	for i, v := range testCollectionNames {
		colls[i] = session.Database(testDB).Collection(v)
	}

	var n int

	// Insert a doc to ensure the DB is actually ready for inserts
	// and not pausing while a dropDatabase is processing.
	_, err = colls[0].InsertOne(context.Background(), bson.M{"n": n})
	if err != nil {
		errs <- err
		close(ready)
		return
	}
	close(ready)
	n++

	for {
		select {
		case <-done:
			return
		default:
			coll := colls[rand.Intn(len(colls))]
			_, err := coll.InsertOne(context.Background(), bson.M{"n": n})
			if err != nil {
				errs <- err
				return
			}
			n++
		}
	}
}

func tearDownMongoDumpTestData() error {
	session, err := testutil.GetBareSession()
	if err != nil {
		return err
	}

	err = session.Database(testDB).Drop(context.Background())
	if err != nil {
		return err
	}
	return nil
}

func dropDB(dbName string) error {
	session, err := testutil.GetBareSession()
	if err != nil {
		return err
	}

	err = session.Database(dbName).Drop(context.Background())
	if err != nil {
		return err
	}
	return nil
}

func fileDirExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

func testQuery(md *MongoDump, session *mongo.Client) string {
	origDB := session.Database(testDB)
	restoredDB := session.Database(testRestoreDB)

	// query to test --query* flags
	bsonQuery := bson.M{"age": bson.M{"$lt": 10}}

	// we can only dump using query per collection
	for _, testCollName := range testCollectionNames {
		md.ToolOptions.Namespace.Collection = testCollName

		err := md.Init()
		So(err, ShouldBeNil)

		err = md.Dump()
		So(err, ShouldBeNil)
	}

	path, err := os.Getwd()
	So(err, ShouldBeNil)

	dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
	dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, testDB))
	So(fileDirExists(dumpDir), ShouldBeTrue)
	So(fileDirExists(dumpDBDir), ShouldBeTrue)

	So(restoredDB.Drop(context.Background()), ShouldBeNil)
	err = readBSONIntoDatabase(dumpDBDir, testRestoreDB)
	So(err, ShouldBeNil)

	for _, testCollName := range testCollectionNames {
		// count filtered docs
		origDocCount, err := origDB.Collection(testCollName).
			CountDocuments(context.Background(), bsonQuery)
		So(err, ShouldBeNil)

		// count number of all restored documents
		restDocCount, err := restoredDB.Collection(testCollName).
			CountDocuments(context.Background(), bson.D{})
		So(err, ShouldBeNil)

		So(restDocCount, ShouldEqual, origDocCount)
	}
	return dumpDir
}

func testDumpOneCollection(md *MongoDump, dumpDir string) {
	path, err := os.Getwd()
	So(err, ShouldBeNil)

	absDumpDir := util.ToUniversalPath(filepath.Join(path, dumpDir))
	So(os.RemoveAll(absDumpDir), ShouldBeNil)
	So(fileDirExists(absDumpDir), ShouldBeFalse)

	dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, testDB))
	So(fileDirExists(dumpDBDir), ShouldBeFalse)

	md.OutputOptions.Out = dumpDir
	err = md.Dump()
	So(err, ShouldBeNil)
	So(fileDirExists(dumpDBDir), ShouldBeTrue)

	session, err := testutil.GetBareSession()
	So(err, ShouldBeNil)

	countColls, err := countNonIndexBSONFiles(dumpDBDir)
	So(err, ShouldBeNil)
	So(countColls, ShouldEqual, 1)

	collOriginal := session.Database(testDB).Collection(md.ToolOptions.Namespace.Collection)

	So(session.Database(testRestoreDB).Drop(context.Background()), ShouldBeNil)
	collRestore := session.Database(testRestoreDB).Collection(md.ToolOptions.Namespace.Collection)

	err = readBSONIntoDatabase(dumpDBDir, testRestoreDB)
	So(err, ShouldBeNil)

	Convey("with the correct number of documents", func() {
		numDocsOrig, err := collOriginal.CountDocuments(context.Background(), bson.D{})
		So(err, ShouldBeNil)

		numDocsRestore, err := collRestore.CountDocuments(context.Background(), bson.D{})
		So(err, ShouldBeNil)

		So(numDocsRestore, ShouldEqual, numDocsOrig)
	})

	Convey("that are the same as the documents in the test database", func() {
		iter, err := collOriginal.Find(context.Background(), bson.D{})
		So(err, ShouldBeNil)

		var result bson.D
		for iter.Next(context.Background()) {
			So(iter.Decode(&result), ShouldBeNil)
			restoredCount, err := collRestore.CountDocuments(context.Background(), result)
			So(err, ShouldBeNil)
			So(restoredCount, ShouldNotEqual, 0)
		}
		So(iter.Err(), ShouldBeNil)
		So(iter.Close(context.Background()), ShouldBeNil)
	})
}

func TestMongoDumpValidateOptions(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.UnitTestType)

	Convey("With a MongoDump instance", t, func() {
		md := simpleMongoDumpInstance()

		Convey("we cannot dump a collection when a database specified", func() {
			md.ToolOptions.Namespace.Collection = "some_collection"
			md.ToolOptions.Namespace.DB = ""

			err := md.ValidateOptions()
			So(err, ShouldNotBeNil)
			So(
				err.Error(),
				ShouldContainSubstring,
				"cannot dump a collection without a specified database",
			)
		})

		Convey("we have to specify a collection name if using a query", func() {
			md.ToolOptions.Namespace.Collection = ""
			md.OutputOptions.Out = ""
			md.InputOptions.Query = "{_id:\"\"}"

			err := md.ValidateOptions()
			So(err, ShouldNotBeNil)
			So(
				err.Error(),
				ShouldContainSubstring,
				"cannot dump using a query without a specified collection",
			)
		})

	})
}

func TestMongoDumpConnectedToAtlasProxy(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	require.NoError(t, err)
	defer sessionProvider.Close()

	md := simpleMongoDumpInstance()
	md.isAtlasProxy = true
	md.OutputOptions.DumpDBUsersAndRoles = false
	md.SessionProvider = sessionProvider

	session, err := sessionProvider.GetSession()
	require.NoError(t, err)
	// This case shouldn't error and should instead not return that it will try to restore users and roles.
	_, err = session.Database("admin").
		Collection("testcol").
		InsertOne(context.Background(), bson.M{})
	require.NoError(t, err)
	dbNames, err := md.GetValidDbs()
	require.NoError(t, err)
	require.NotContains(t, dbNames, "admin")

	// This case should error because it has explicitly been set to dump users and roles for a DB, but thats
	// not possible with an atlas proxy.
	md.OutputOptions.DumpDBUsersAndRoles = true
	err = md.ValidateOptions()
	require.Error(
		t,
		err,
		"can't dump from admin database when connecting to a MongoDB Atlas free or shared cluster",
	)

	require.NoError(t, session.Database("admin").Collection("testcol").Drop(context.Background()))
}

func TestMongoDumpBSON(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	Convey("With a MongoDump instance", t, func() {
		err := setUpMongoDumpTestData()
		So(err, ShouldBeNil)

		Convey(
			"testing that using MongoDump WITHOUT giving a query dumps everything in the database and/or collection",
			func() {
				md := simpleMongoDumpInstance()
				md.InputOptions.Query = ""

				Convey("and that for a particular collection", func() {
					md.ToolOptions.Namespace.Collection = testCollectionNames[0]
					err = md.Init()
					So(err, ShouldBeNil)

					Convey("it dumps to the default output directory", func() {
						testDumpOneCollection(md, "dump")
					})

					Convey("it dumps to a user-specified output directory", func() {
						testDumpOneCollection(md, "dump_user")
					})

					Convey("it dumps to standard output", func() {
						md.OutputOptions.Out = "-"
						stdoutBuf := &bytes.Buffer{}
						md.OutputWriter = stdoutBuf
						err = md.Dump()
						So(err, ShouldBeNil)
						var count int
						bsonSource := db.NewDecodedBSONSource(
							db.NewBSONSource(io.NopCloser(stdoutBuf)),
						)
						defer bsonSource.Close()

						var result bson.Raw
						for bsonSource.Next(&result) {
							count++
						}
						So(bsonSource.Err(), ShouldBeNil)
						So(count, ShouldEqual, 10) //The 0th collection has 10 documents

						Reset(func() {
						})

					})

				})

				Convey("and that it dumps a collection with a slash in its name", func() {
					md.ToolOptions.Namespace.Collection = testCollectionNames[2]

					Convey("to the filesystem", func() {
						err = md.Init()
						So(err, ShouldBeNil)
						testDumpOneCollection(md, "dump_slash")
					})

					Convey("to an archive", func() {
						md.OutputOptions.Archive = "dump_slash.archive"
						err = md.Init()
						So(err, ShouldBeNil)
					})
				})

				Convey("for an entire database", func() {
					md.ToolOptions.Namespace.Collection = ""
					err = md.Init()
					So(err, ShouldBeNil)

					Convey(
						"that exists. The dumped directory should contain the necessary bson files",
						func() {
							md.OutputOptions.Out = "dump"
							err = md.Dump()
							So(err, ShouldBeNil)
							path, err := os.Getwd()
							So(err, ShouldBeNil)

							dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
							dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, testDB))
							So(fileDirExists(dumpDir), ShouldBeTrue)
							So(fileDirExists(dumpDBDir), ShouldBeTrue)

							countColls, err := countNonIndexBSONFiles(dumpDBDir)
							So(err, ShouldBeNil)
							So(countColls, ShouldEqual, len(testCollectionNames))

							Reset(func() {
								So(os.RemoveAll(dumpDir), ShouldBeNil)
							})

						},
					)

					Convey(
						"that does not exist. The dumped directory shouldn't be created",
						func() {
							md.OutputOptions.Out = "dump"
							md.ToolOptions.Namespace.DB = "nottestdb"
							err = md.Dump()
							So(err, ShouldBeNil)

							path, err := os.Getwd()
							So(err, ShouldBeNil)

							dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
							dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, "nottestdb"))

							So(fileDirExists(dumpDir), ShouldBeFalse)
							So(fileDirExists(dumpDBDir), ShouldBeFalse)
						},
					)

				})
			},
		)

		Convey(
			"testing that using MongoDump WITH a query dumps a subset of documents in a database and/or collection",
			func() {
				session, err := testutil.GetBareSession()
				So(err, ShouldBeNil)
				md := simpleMongoDumpInstance()

				// expect 10 documents per collection
				bsonQuery := bson.M{"age": bson.M{"$lt": 10}}
				jsonQuery, err := bsonutil.ConvertBSONValueToLegacyExtJSON(bsonQuery)
				So(err, ShouldBeNil)
				jsonQueryBytes, err := json.Marshal(jsonQuery)
				So(err, ShouldBeNil)

				Convey("using --query for all the collections in the database", func() {
					md.InputOptions.Query = string(jsonQueryBytes)
					md.ToolOptions.Namespace.DB = testDB
					md.OutputOptions.Out = "dump"
					dumpDir := testQuery(md, session)

					Reset(func() {
						So(session.Database(testRestoreDB).Drop(context.Background()), ShouldBeNil)
						So(os.RemoveAll(dumpDir), ShouldBeNil)
					})

				})

				Convey("using --queryFile for all the collections in the database", func() {
					err = os.WriteFile("example.json", jsonQueryBytes, 0777)
					So(err, ShouldBeNil)
					md.InputOptions.QueryFile = "example.json"
					md.ToolOptions.Namespace.DB = testDB
					md.OutputOptions.Out = "dump"
					dumpDir := testQuery(md, session)

					Reset(func() {
						So(session.Database(testRestoreDB).Drop(context.Background()), ShouldBeNil)
						So(os.RemoveAll(dumpDir), ShouldBeNil)
						So(os.Remove("example.json"), ShouldBeNil)
					})

				})
			},
		)

		Convey("using MongoDump against a collection that doesn't exist succeeds", func() {
			md := simpleMongoDumpInstance()
			md.ToolOptions.Namespace.DB = "nonExistentDB"
			md.ToolOptions.Namespace.Collection = "nonExistentColl"

			err := md.Init()
			So(err, ShouldBeNil)
			err = md.Dump()
			So(err, ShouldBeNil)
		})

		Reset(func() {
			So(tearDownMongoDumpTestData(), ShouldBeNil)
		})
	})
}

func TestMongoDumpBSONLongCollectionName(t *testing.T) {
	// Disabled: see TOOLS-2658
	t.Skip()

	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}
	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "4.4"); err != nil || cmp < 0 {
		t.Skipf("Requires server with FCV 4.4 or later; found %v", fcv)
	}

	log.SetWriter(io.Discard)

	Convey("With a MongoDump instance", t, func() {
		err = setUpMongoDumpTestData()
		So(err, ShouldBeNil)

		md := simpleMongoDumpInstance()

		Convey(
			"testing that it dumps a collection with a name >238 bytes in the right format",
			func() {
				coll := session.Database(testDB).Collection(longCollectionName)
				_, err = coll.InsertOne(context.Background(), bson.M{"a": 1})
				So(err, ShouldBeNil)
				//nolint:errcheck
				defer coll.Drop(context.Background())

				md.ToolOptions.Namespace.Collection = longCollectionName
				err = md.Init()
				So(err, ShouldBeNil)

				path, err := os.Getwd()
				So(err, ShouldBeNil)

				absDumpDir := util.ToUniversalPath(filepath.Join(path, "dump_slash"))
				So(os.RemoveAll(absDumpDir), ShouldBeNil)
				So(fileDirExists(absDumpDir), ShouldBeFalse)

				dumpDBDir := util.ToUniversalPath(filepath.Join("dump_slash", testDB))
				So(fileDirExists(dumpDBDir), ShouldBeFalse)

				md.OutputOptions.Out = "dump_slash"
				err = md.Dump()
				So(err, ShouldBeNil)
				So(fileDirExists(dumpDBDir), ShouldBeTrue)

				Convey("to a bson file", func() {
					oneBsonFile, err := os.Open(
						util.ToUniversalPath(filepath.Join(dumpDBDir, longBsonName)),
					)
					So(err, ShouldBeNil)
					oneBsonFile.Close()
				})

				Convey("to a metadata file", func() {
					oneMetaFile, err := os.Open(
						util.ToUniversalPath(filepath.Join(dumpDBDir, longMetadataName)),
					)
					So(err, ShouldBeNil)
					oneMetaFile.Close()
				})
			},
		)

		Reset(func() {
			So(tearDownMongoDumpTestData(), ShouldBeNil)
		})
	})
}

func testPreludeMetadata(md *MongoDump, dir string, serverVersion string) {
	So(fileDirExists(dir), ShouldBeFalse)
	err := md.Init()
	So(err, ShouldBeNil)

	err = md.Dump()
	So(err, ShouldBeNil)

	preludeFilepath := filepath.Join(dir, "prelude.json")
	if md.OutputOptions.Gzip {
		preludeFilepath += ".gz"
	}
	So(fileDirExists(preludeFilepath), ShouldBeTrue)
	var reader io.Reader
	preludeFile, err := os.Open(util.ToUniversalPath(preludeFilepath))
	So(err, ShouldBeNil)
	reader = preludeFile
	defer preludeFile.Close()
	if md.OutputOptions.Gzip {
		zipfile, err := gzip.NewReader(preludeFile)
		So(err, ShouldBeNil)
		defer zipfile.Close()
		reader = zipfile
	}
	contents, err := io.ReadAll(reader)
	So(err, ShouldBeNil)
	var jsonResult map[string]string
	err = json.Unmarshal(contents, &jsonResult)
	So(err, ShouldBeNil)
	So(jsonResult["ServerVersion"], ShouldEqual, serverVersion)
}

func TestDumpPreludeMetadataJson(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	Convey("With a MongoDump instance", t, func() {
		path, err := os.Getwd()
		So(err, ShouldBeNil)

		err = setUpMongoDumpTestData()
		So(err, ShouldBeNil)

		sessionProvider, _, _ := testutil.GetBareSessionProvider()
		So(sessionProvider, ShouldNotBeNil)
		serverVersion, err := sessionProvider.ServerVersion()
		So(err, ShouldBeNil)

		Convey("when dumping all databases", func() {
			md := simpleMongoDumpInstance()
			md.ToolOptions.Namespace.DB = ""
			md.ToolOptions.Namespace.Collection = ""

			Convey("when dumping to the default directory", func() {
				dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
				So(os.RemoveAll(dumpDir), ShouldBeNil)

				Convey("writes prelude.json to dump directory", func() {
					testPreludeMetadata(md, dumpDir, serverVersion)
				})

				Convey("writes prelude.json.gz to dump directory when --gzip is used", func() {
					md.OutputOptions.Gzip = true
					testPreludeMetadata(md, dumpDir, serverVersion)
				})

				Reset(func() {
					So(os.RemoveAll(dumpDir), ShouldBeNil)
				})
			})

			Convey("when output directory is specified", func() {
				dumpDir := util.ToUniversalPath(filepath.Join(path, "dump_output"))
				So(os.RemoveAll(dumpDir), ShouldBeNil)

				Convey("writes prelude.json to output directory", func() {
					md.OutputOptions.Out = "dump_output"
					testPreludeMetadata(md, dumpDir, serverVersion)
				})

				Reset(func() {
					So(os.RemoveAll(dumpDir), ShouldBeNil)
				})
			})
		})

		Convey("when dumping one db", func() {
			md := simpleMongoDumpInstance()
			dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
			dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, testDB))
			So(os.RemoveAll(dumpDir), ShouldBeNil)

			Convey("writes prelude.json to dump directory", func() {
				testPreludeMetadata(md, dumpDBDir, serverVersion)
			})

			Reset(func() {
				So(os.RemoveAll(dumpDir), ShouldBeNil)
			})
		})

		Convey("when the dump directory is not created", func() {

			dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
			dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, "nottestdb"))

			Convey("the dump does not fail and prelude.json should not be created", func() {
				md := simpleMongoDumpInstance()
				md.ToolOptions.Namespace.DB = "nonExistentDB"

				err := md.Init()
				So(err, ShouldBeNil)
				err = md.Dump()
				So(err, ShouldBeNil)

				So(fileDirExists(dumpDir), ShouldBeFalse)
				So(fileDirExists(dumpDBDir), ShouldBeFalse)
				So(fileDirExists(filepath.Join(dumpDir, "prelude.json")), ShouldBeFalse)
				So(fileDirExists(filepath.Join(dumpDBDir, "prelude.json")), ShouldBeFalse)
			})

			Reset(func() {
				So(os.RemoveAll(dumpDir), ShouldBeNil)
			})
		})

		Reset(func() {
			So(tearDownMongoDumpTestData(), ShouldBeNil)
		})

	})
}

func TestMongoDumpMetaData(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	Convey("With a MongoDump instance", t, func() {
		session, err := testutil.GetBareSession()
		So(session, ShouldNotBeNil)
		So(err, ShouldBeNil)

		err = setUpMongoDumpTestData()
		So(err, ShouldBeNil)

		Convey("testing that the dumped directory contains information about indexes", func() {

			md := simpleMongoDumpInstance()
			md.OutputOptions.Out = "dump"
			err = md.Init()
			So(err, ShouldBeNil)

			err = md.Dump()
			So(err, ShouldBeNil)

			path, err := os.Getwd()
			So(err, ShouldBeNil)
			dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
			dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, testDB))
			So(fileDirExists(dumpDir), ShouldBeTrue)
			So(fileDirExists(dumpDBDir), ShouldBeTrue)

			Convey("having one metadata file per collection", func() {
				c1, err := countNonIndexBSONFiles(dumpDBDir)
				So(err, ShouldBeNil)

				c2, err := countMetaDataFiles(dumpDBDir)
				So(err, ShouldBeNil)

				So(c1, ShouldEqual, c2)

				Convey("and that the JSON in a metadata file is valid", func() {
					metaFiles, err := getMatchingFiles(dumpDBDir, ".*\\.metadata\\.json")
					So(err, ShouldBeNil)
					So(len(metaFiles), ShouldBeGreaterThan, 0)

					oneMetaFile, err := os.Open(
						util.ToUniversalPath(filepath.Join(dumpDBDir, metaFiles[0])),
					)
					//nolint:staticcheck
					defer oneMetaFile.Close()
					So(err, ShouldBeNil)
					contents, err := io.ReadAll(oneMetaFile)
					So(err, ShouldBeNil)
					var jsonResult map[string]interface{}
					err = json.Unmarshal(contents, &jsonResult)
					So(err, ShouldBeNil)

					Convey("and contains an 'indexes' key", func() {
						_, ok := jsonResult["indexes"]
						So(ok, ShouldBeTrue)
					})

					Convey("and contains a 'collectionName' key", func() {
						_, ok := jsonResult["collectionName"]
						So(ok, ShouldBeTrue)
					})

					fcv := testutil.GetFCV(session)
					cmp, err := testutil.CompareFCV(fcv, "3.6")
					So(err, ShouldBeNil)
					if cmp >= 0 {
						Convey("and on FCV 3.6+, contains a 'uuid' key", func() {
							uuid, ok := jsonResult["uuid"]
							So(ok, ShouldBeTrue)
							checkUUID := regexp.MustCompile(`(?i)^[a-z0-9]{32}$`)

							uuidStr, ok := uuid.(string)
							So(ok, ShouldBeTrue)

							So(checkUUID.MatchString(uuidStr), ShouldBeTrue)
							// XXX useless -- xdg, 2018-09-21
							So(err, ShouldBeNil)
						})
					}

				})

			})

			Reset(func() {
				So(os.RemoveAll(dumpDir), ShouldBeNil)
			})
		})

		Reset(func() {
			So(tearDownMongoDumpTestData(), ShouldBeNil)
		})

	})
}

func TestMongoDumpOplog(t *testing.T) {
	// Disabled: see TOOLS-2657
	t.Skip()

	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}
	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("No client available: %v", err)
	}
	if ok, _ := sessionProvider.IsReplicaSet(); !ok {
		t.SkipNow()
	}
	log.SetWriter(io.Discard)

	Convey("With a MongoDump instance", t, func() {

		Convey("testing that the dumped directory contains an oplog", func() {

			// Start with clean filesystem
			path, err := os.Getwd()
			So(err, ShouldBeNil)

			dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
			dumpOplogFile := util.ToUniversalPath(filepath.Join(dumpDir, "oplog.bson"))

			err = os.RemoveAll(dumpDir)
			So(err, ShouldBeNil)
			So(fileDirExists(dumpDir), ShouldBeFalse)

			// Start with clean database
			So(tearDownMongoDumpTestData(), ShouldBeNil)

			// Prepare mongodump with options
			md := simpleMongoDumpInstance()
			md.OutputOptions.Oplog = true
			md.ToolOptions.Namespace = &options.Namespace{}
			err = md.Init()
			So(err, ShouldBeNil)

			// Start inserting docs in the background so the oplog has data
			ready := make(chan struct{})
			done := make(chan struct{})
			errs := make(chan error, 1)
			go backgroundInsert(ready, done, errs)
			<-ready

			// Run mongodump
			err = md.Dump()
			So(err, ShouldBeNil)

			// Stop background insertion
			close(done)
			err = <-errs
			So(err, ShouldBeNil)

			// Check for and read the oplog file
			So(fileDirExists(dumpDir), ShouldBeTrue)
			So(fileDirExists(dumpOplogFile), ShouldBeTrue)

			oplogFile, err := os.Open(dumpOplogFile)
			//nolint:staticcheck
			defer oplogFile.Close()
			So(err, ShouldBeNil)

			rdr := db.NewBSONSource(oplogFile)
			iter := db.NewDecodedBSONSource(rdr)

			fcv := testutil.GetFCV(session)
			cmp, err := testutil.CompareFCV(fcv, "3.6")
			So(err, ShouldBeNil)

			withUI := countOplogUI(iter)
			So(iter.Err(), ShouldBeNil)

			if cmp >= 0 {
				// for FCV 3.6+, should have 'ui' field in oplog entries
				So(withUI, ShouldBeGreaterThan, 0)
			} else {
				// for FCV <3.6, should no have 'ui' field in oplog entries
				So(withUI, ShouldEqual, 0)
			}

			// Cleanup
			So(os.RemoveAll(dumpDir), ShouldBeNil)
			So(tearDownMongoDumpTestData(), ShouldBeNil)
		})

	})
}

// Test dumping a collection with autoIndexId:false.  As of MongoDB 4.0,
// this is only allowed on the 'local' database.
func TestMongoDumpTOOLS2174(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}

	collName := "tools-2174"
	dbName := "local"

	var r1 bson.M
	err = sessionProvider.Run(bson.D{{"drop", collName}}, &r1, dbName)
	if err != nil {
		var commandErr mongo.CommandError
		if !(errors.As(err, &commandErr) && commandErr.Code == 26) {
			t.Fatalf("Failed to run drop: %v", err)
		}
	}

	createCmd := bson.D{
		{"create", collName},
		{"autoIndexId", false},
	}
	var r2 bson.M
	err = sessionProvider.Run(createCmd, &r2, dbName)
	if err != nil {
		t.Fatalf("Error creating capped, no-autoIndexId collection: %v", err)
	}

	Convey("testing dumping a capped, autoIndexId:false collection", t, func() {
		md := simpleMongoDumpInstance()
		md.ToolOptions.Namespace.Collection = collName
		md.ToolOptions.Namespace.DB = dbName
		md.OutputOptions.Out = "dump"
		err = md.Init()
		So(err, ShouldBeNil)
		err = md.Dump()
		So(err, ShouldBeNil)
	})
}

// Test dumping a collection while respecting no index scan for wired tiger.
func TestMongoDumpTOOLS1952(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}

	collName := "tools-1952-dump"
	dbName := "test"
	ns := dbName + "." + collName

	var r1 bson.M

	dbStruct := session.Database(dbName)

	err = sessionProvider.Run(bson.D{{"drop", collName}}, &r1, dbName)
	if err != nil {
		var commandErr mongo.CommandError
		if !(errors.As(err, &commandErr) && commandErr.Code == 26) {
			t.Fatalf("Failed to run drop: %v", err)
		}
	}

	createCmd := bson.D{
		{"create", collName},
	}
	var r2 bson.M
	err = sessionProvider.Run(createCmd, &r2, dbName)
	if err != nil {
		t.Fatalf("Error creating collection: %v", err)
	}

	// Turn on profiling.
	if err = turnOnProfiling(dbName); err != nil {
		t.Fatalf("Failed to turn on profiling: %v", err)
	}

	profileCollection := dbStruct.Collection("system.profile")

	Convey("testing dumping a collection query hints", t, func() {
		md := simpleMongoDumpInstance()
		md.ToolOptions.Namespace.Collection = collName
		md.ToolOptions.Namespace.DB = dbName
		md.OutputOptions.Out = "dump"
		err = md.Init()
		So(err, ShouldBeNil)
		err = md.Dump()
		So(err, ShouldBeNil)

		count, err := countSnapshotCmds(profileCollection, ns)
		So(err, ShouldBeNil)

		// On modern storage engines, there should be no query that matches.
		So(count, ShouldEqual, 0)
	})
}

// Test the fix for nil pointer bug when getCollectionInfo failed.
func TestMongoDumpTOOLS2498(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}

	collName := "tools-2498-dump"
	dbName := "test"

	var r1 bson.M
	err = sessionProvider.Run(bson.D{{"drop", collName}}, &r1, dbName)
	if err != nil {
		var commandErr mongo.CommandError
		if !(errors.As(err, &commandErr) && commandErr.Code == 26) {
			t.Fatalf("Failed to run drop: %v", err)
		}
	}

	createCmd := bson.D{
		{"create", collName},
	}
	var r2 bson.M
	err = sessionProvider.Run(createCmd, &r2, dbName)
	if err != nil {
		t.Fatalf("Error creating collection: %v", err)
	}

	Convey("failing to get collection info should error, but not panic", t, func() {
		md := simpleMongoDumpInstance()
		md.ToolOptions.Namespace.Collection = collName
		md.ToolOptions.Namespace.DB = dbName
		md.OutputOptions.Out = "dump"
		err = md.Init()
		So(err, ShouldBeNil)

		failpoint.ParseFailpoints("PauseBeforeDumping")
		defer failpoint.Reset()

		var disconnectErr error
		// with the failpoint PauseBeforeDumping, Mongodump will pause 15 seconds before starting dumping. We will close the connection
		// during this period. Before the fix, the process will panic with Nil pointer error since it fails to getCollectionInfo.
		go func() {
			time.Sleep(2 * time.Second)
			session, _ := md.SessionProvider.GetSession()
			disconnectErr = session.Disconnect(context.Background())
		}()

		err = md.Dump()
		// Mongodump should not panic, but return correct error if failed to getCollectionInfo
		So(err, ShouldNotBeNil)
		So(err.Error(), ShouldContainSubstring, "client is disconnected")

		So(disconnectErr, ShouldBeNil)
	})
}

func TestMongoDumpOrderedQuery(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	Convey("With a MongoDump instance", t, func() {
		err := setUpMongoDumpTestData()
		So(err, ShouldBeNil)
		path, err := os.Getwd()
		So(err, ShouldBeNil)
		dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))

		Convey("testing that --query is order-preserving", func() {
			// If order is not preserved, probabalistically, some of these
			// loops will fail.
			for i := 0; i < 100; i++ {
				So(os.RemoveAll(dumpDir), ShouldBeNil)

				md := simpleMongoDumpInstance()
				md.InputOptions.Query = `{"coords":{"x":0,"y":1}}`
				md.ToolOptions.Namespace.Collection = testCollectionNames[0]
				md.ToolOptions.Namespace.DB = testDB
				md.OutputOptions.Out = "dump"
				err = md.Init()
				So(err, ShouldBeNil)
				err = md.Dump()
				So(err, ShouldBeNil)

				dumpBSON := util.ToUniversalPath(
					filepath.Join(dumpDir, testDB, testCollectionNames[0]+".bson"),
				)

				file, err := os.Open(dumpBSON)
				So(err, ShouldBeNil)

				bsonSource := db.NewDecodedBSONSource(db.NewBSONSource(file))

				var count int
				var result bson.M
				for bsonSource.Next(&result) {
					count++
				}
				So(bsonSource.Err(), ShouldBeNil)

				So(count, ShouldEqual, 1)

				bsonSource.Close()
				file.Close()
			}
		})

		Reset(func() {
			So(os.RemoveAll(dumpDir), ShouldBeNil)
			So(tearDownMongoDumpTestData(), ShouldBeNil)
		})
	})
}

func TestMongoDumpViewsAsCollections(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	Convey("With a MongoDump instance", t, func() {
		err := setUpMongoDumpTestData()
		So(err, ShouldBeNil)

		colName := "dump_view_as_collection"
		dbName := testDB
		err = setUpDBView(dbName, colName)
		So(err, ShouldBeNil)

		err = turnOnProfiling(testDB)
		So(err, ShouldBeNil)

		Convey("testing that the dumped directory contains information about metadata", func() {
			md := simpleMongoDumpInstance()
			md.ToolOptions.Namespace.DB = testDB
			md.OutputOptions.Out = "dump"
			md.OutputOptions.ViewsAsCollections = true

			err = md.Init()
			So(err, ShouldBeNil)

			err = md.Dump()
			So(err, ShouldBeNil)

			path, err := os.Getwd()
			So(err, ShouldBeNil)

			dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
			dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, testDB))
			So(fileDirExists(dumpDir), ShouldBeTrue)
			So(fileDirExists(dumpDBDir), ShouldBeTrue)

			Convey("having one metadata file per read-only view", func() {
				c1, err := countNonIndexBSONFiles(dumpDBDir)
				So(err, ShouldBeNil)

				c2, err := countMetaDataFiles(dumpDBDir)
				So(err, ShouldBeNil)

				So(c1, ShouldEqual, c2)

			})

			Convey("testing dumping a view, we should not hint index", func() {
				session, err := testutil.GetBareSession()
				So(err, ShouldBeNil)

				dbStruct := session.Database(dbName)
				profileCollection := dbStruct.Collection("system.profile")
				ns := dbName + "." + colName
				count, err := countSnapshotCmds(profileCollection, ns)
				So(err, ShouldBeNil)

				// view dump should not do collection scan
				So(count, ShouldEqual, 0)
			})

			Reset(func() {
				So(os.RemoveAll(dumpDir), ShouldBeNil)
			})
		})

		Reset(func() {
			So(tearDownMongoDumpTestData(), ShouldBeNil)
		})

	})
}

func TestMongoDumpViews(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	Convey("With a MongoDump instance", t, func() {
		err := setUpMongoDumpTestData()
		So(err, ShouldBeNil)

		colName := "dump_views"
		dbName := testDB
		err = setUpDBView(dbName, colName)
		So(err, ShouldBeNil)

		Convey("testing that the dumped directory contains information about metadata", func() {

			md := simpleMongoDumpInstance()
			md.ToolOptions.Namespace.DB = testDB
			md.OutputOptions.Out = "dump"

			err = md.Init()
			So(err, ShouldBeNil)

			err = md.Dump()
			So(err, ShouldBeNil)

			path, err := os.Getwd()
			So(err, ShouldBeNil)

			dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
			dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, testDB))
			So(fileDirExists(dumpDir), ShouldBeTrue)
			So(fileDirExists(dumpDBDir), ShouldBeTrue)

			Convey("having one metadata file per view", func() {

				c1, err := countMetaDataFiles(dumpDBDir)
				So(err, ShouldBeNil)

				So(c1, ShouldBeGreaterThan, 0)

			})

			Convey("testing dumping a view, we should not hint index", func() {
				session, err := testutil.GetBareSession()
				So(err, ShouldBeNil)

				dbStruct := session.Database(dbName)
				profileCollection := dbStruct.Collection("system.profile")
				ns := dbName + "." + colName
				count, err := countSnapshotCmds(profileCollection, ns)
				So(err, ShouldBeNil)

				// view dump should not do collection scan
				So(count, ShouldEqual, 0)
			})

			Reset(func() {
				So(os.RemoveAll(dumpDir), ShouldBeNil)
			})
		})

		Reset(func() {
			So(tearDownMongoDumpTestData(), ShouldBeNil)
		})

	})
}

func TestMongoDumpCollectionOutputPath(t *testing.T) {
	// Disabled: see TOOLS-2658
	t.Skip()

	testtype.SkipUnlessTestType(t, testtype.UnitTestType)
	log.SetWriter(io.Discard)

	Convey("testing output paths for collection names of varying lengths", t, func() {
		md := simpleMongoDumpInstance()

		Convey("don't change a collection name that results in an output path <255 bytes", func() {
			md.OutputOptions.Out = "dump"

			// 26 bytes < 255 bytes
			// (output path will be under 255 bytes, regardless of file extension)
			colName := "abcdefghijklmnopqrstuvwxyz"

			fileComponents := strings.Split(md.outputPath(testDB, colName), "/")
			So(len(fileComponents), ShouldEqual, 3)

			filePath := fileComponents[len(fileComponents)-1]
			So(filePath, ShouldEqual, colName)
			So(filePath, ShouldNotContainSubstring, "%24")
		})

		Convey(
			"don't change a collection name that could result in an output path =255 bytes",
			func() {
				md.OutputOptions.Out = "dump"

				// 17 bytes * 14 = 238 bytes
				// (output would be exactly 255 bytes with longest possible file extension of .metadata.json.gz)
				colName := strings.Repeat("abcdefghijklmnopq", 14)

				fileComponents := strings.Split(md.outputPath(testDB, colName), "/")
				So(len(fileComponents), ShouldEqual, 3)

				filePath := fileComponents[len(fileComponents)-1]
				So(filePath, ShouldEqual, colName)
				So(filePath, ShouldNotContainSubstring, "%24")
			},
		)

		Convey("truncate a collection name that results in an output path >255 bytes", func() {
			md.OutputOptions.Out = "dump"

			Convey("without special characters", func() {
				// 26 bytes * 10 = 260 bytes > 238 bytes
				// (output path is already over the file name limit of 255, regardless of file extension)
				colName := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 10)

				fileComponents := strings.Split(md.outputPath(testDB, colName), "/")
				So(len(fileComponents), ShouldEqual, 3)

				filePath := fileComponents[len(fileComponents)-1]
				So(filePath[:211], ShouldEqual, colName[:208]+"%24")

				hashDecoded, _ := base64.RawURLEncoding.DecodeString(filePath[211:])
				hash := sha1.Sum([]byte(colName))
				So(bytes.Compare(hashDecoded, hash[:]), ShouldEqual, 0)
			})

			Convey("with special characters", func() {
				// (26 bytes + 3 special bytes) * 8 = 232 bytes < 238 bytes
				// (output path is under the limit, but will go over when we escape the special symbols)
				colName := strings.Repeat("abcdefghijklmnopqrstuvwxyz+/@", 8)

				fileComponents := strings.Split(md.outputPath(testDB, colName), "/")
				So(len(fileComponents), ShouldEqual, 3)

				filePath := fileComponents[len(fileComponents)-1]
				So(filePath[:211], ShouldEqual, util.EscapeCollectionName(colName)[:208]+"%24")

				hashDecoded, _ := base64.RawURLEncoding.DecodeString(filePath[211:])
				hash := sha1.Sum([]byte(colName))
				So(bytes.Compare(hashDecoded, hash[:]), ShouldEqual, 0)
			})
		})
	})
}

func TestCount(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	Convey("test count collection", t, func() {
		err := setUpMongoDumpTestData()
		So(err, ShouldBeNil)

		session, err := testutil.GetBareSession()
		So(err, ShouldBeNil)

		collection := session.Database(testDB).Collection(testCollectionNames[0])
		restoredDB := session.Database(testDB)
		//nolint:errcheck
		defer restoredDB.Drop(context.Background())

		Convey("count collection without filter", func() {
			findQuery := &db.DeferredQuery{Coll: collection}
			cnt, err := findQuery.Count(false)
			So(err, ShouldBeNil)
			So(cnt, ShouldEqual, 10)

			findQuery = &db.DeferredQuery{Coll: collection, Filter: bson.M{}}
			cnt, err = findQuery.Count(false)
			So(err, ShouldBeNil)
			So(cnt, ShouldEqual, 10)

			findQuery = &db.DeferredQuery{Coll: collection, Filter: bson.D{}}
			cnt, err = findQuery.Count(false)
			So(err, ShouldBeNil)
			So(cnt, ShouldEqual, 10)
		})

		Convey("count collection with filter in BSON.M", func() {
			findQuery := &db.DeferredQuery{Coll: collection, Filter: bson.M{"age": 1}}
			cnt, err := findQuery.Count(false)
			So(err, ShouldBeNil)
			So(cnt, ShouldEqual, 1)
		})

		Convey("count collection with filter in BSON.D", func() {
			findQuery := &db.DeferredQuery{Coll: collection, Filter: bson.D{{"age", 1}}}
			cnt, err := findQuery.Count(false)
			So(err, ShouldBeNil)
			So(cnt, ShouldEqual, 1)
		})
	})
}

func TestTimeseriesCollections(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}
	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "5.0"); err != nil || cmp < 0 {
		t.Skipf("Requires server with FCV 5.0 or later; found %v", fcv)
	}

	colName := "timeseriesColl"
	dbName := "timeseries_test_DB"
	err = setUpTimeseries(dbName, colName)
	if err != nil {
		t.Fatalf("Failed to set up timeseries collection: %v", err)
	}

	Convey("With a MongoDump instance", t, func() {

		md := simpleMongoDumpInstance()
		md.ToolOptions.Namespace.DB = dbName
		md.OutputOptions.Out = "dump"

		Convey("a timeseries collection should produce a well-formatted dump", func() {
			Convey("to an archive", func() {

				md.OutputOptions.Out = ""
				md.OutputOptions.Archive = "dump.archive"

				Convey("when dumping the whole database", func() {
					// This is the default test of "to an archive".
					// It's meant to be a noop, do not delete.
				})

				Convey("when the collection is specified in --collection", func() {
					md.ToolOptions.DB = dbName
					md.ToolOptions.Collection = colName
				})

				Convey("even when the system.buckets collection is excluded", func() {
					Convey("by --excludeCollections", func() {
						md.OutputOptions.ExcludedCollections = []string{"system.buckets." + colName}
					})

					Convey("by --excludeCollectionsWithPrefix", func() {
						md.OutputOptions.ExcludedCollectionPrefixes = []string{"system.buckets."}
					})
				})

				err = md.Init()
				So(err, ShouldBeNil)

				err = md.Dump()
				So(err, ShouldBeNil)

				path, err := os.Getwd()
				So(err, ShouldBeNil)

				archiveFilePath := util.ToUniversalPath(filepath.Join(path, "dump.archive"))

				archiveFile, err := os.Open(archiveFilePath)
				So(err, ShouldBeNil)
				archiveReader := &archive.Reader{
					In:      archiveFile,
					Prelude: &archive.Prelude{},
				}

				err = archiveReader.Prelude.Read(archiveReader.In)
				So(err, ShouldBeNil)

				collectionMetadatas, ok := archiveReader.Prelude.NamespaceMetadatasByDB[dbName]
				So(ok, ShouldBeTrue)

				So(len(collectionMetadatas), ShouldEqual, 1)
				So(collectionMetadatas[0].Collection, ShouldEqual, colName)

				pe, err := archiveReader.Prelude.NewPreludeExplorer()
				So(err, ShouldBeNil)

				archiveContents, err := pe.ReadDir()
				So(err, ShouldBeNil)

				for _, dirlike := range archiveContents {
					if dirlike.IsDir() && dirlike.Name() == dbName {
						dbContents, err := dirlike.ReadDir()
						So(err, ShouldBeNil)

						So(len(dbContents), ShouldEqual, 2)

						for _, file := range dbContents {
							So(
								file.Name(),
								ShouldBeIn,
								[]string{
									colName + ".metadata.json",
									"system.buckets." + colName + ".bson",
								},
							)
						}
					}
				}

				So(archiveFile.Close(), ShouldBeNil)
				So(os.RemoveAll(archiveFilePath), ShouldBeNil)
			})

			Convey("to a directory", func() {

				Convey("when dumping the whole database", func() {
					// This is the default test of "to a directory".
					// It's meant to be a noop, do not delete.
				})

				Convey("when the collection is specified in --collection", func() {
					md.ToolOptions.DB = dbName
					md.ToolOptions.Collection = colName
				})

				Convey("even when the system.buckets collection is excluded", func() {
					Convey("by --excludeCollections", func() {
						md.OutputOptions.ExcludedCollections = []string{"system.buckets." + colName}
					})

					Convey("by --excludeCollectionsWithPrefix", func() {
						md.OutputOptions.ExcludedCollectionPrefixes = []string{"system.buckets."}
					})
				})

				err = md.Init()
				So(err, ShouldBeNil)

				err = md.Dump()
				So(err, ShouldBeNil)

				path, err := os.Getwd()
				So(err, ShouldBeNil)

				dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
				dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, dbName))
				metadataFile := util.ToUniversalPath(
					filepath.Join(dumpDBDir, colName+".metadata.json"),
				)
				bsonFile := util.ToUniversalPath(
					filepath.Join(dumpDBDir, "system.buckets."+colName+".bson"),
				)
				So(fileDirExists(dumpDir), ShouldBeTrue)
				So(fileDirExists(dumpDBDir), ShouldBeTrue)
				So(fileDirExists(metadataFile), ShouldBeTrue)
				So(fileDirExists(bsonFile), ShouldBeTrue)

				allFiles, err := getMatchingFiles(dumpDBDir, ".*"+colName+".*")
				So(err, ShouldBeNil)
				So(len(allFiles), ShouldEqual, 2)

				info, err := os.Stat(bsonFile)
				So(err, ShouldBeNil)
				So(info.Size(), ShouldBeGreaterThan, 0)

				So(os.RemoveAll(dumpDir), ShouldBeNil)

			})

		})

		Convey("a timeseries collection should not be dumped", func() {
			Convey("to an archive", func() {

				md.OutputOptions.Out = ""
				md.OutputOptions.Archive = "dump.archive"

				Convey("when the collection is excluded", func() {
					Convey("by --excludeCollections", func() {
						md.ToolOptions.DB = dbName
						md.OutputOptions.ExcludedCollections = []string{colName}
					})

					Convey("by --excludeCollectionsWithPrefix", func() {
						md.ToolOptions.DB = dbName
						md.OutputOptions.ExcludedCollectionPrefixes = []string{colName[:5]}
					})
				})

				err = md.Init()
				So(err, ShouldBeNil)

				err = md.Dump()
				So(err, ShouldBeNil)

				path, err := os.Getwd()
				So(err, ShouldBeNil)

				archiveFilePath := util.ToUniversalPath(filepath.Join(path, "dump.archive"))

				archiveFile, err := os.Open(archiveFilePath)
				So(err, ShouldBeNil)
				archiveReader := &archive.Reader{
					In:      archiveFile,
					Prelude: &archive.Prelude{},
				}

				err = archiveReader.Prelude.Read(archiveReader.In)
				So(err, ShouldBeNil)

				_, ok := archiveReader.Prelude.NamespaceMetadatasByDB[dbName]
				So(ok, ShouldBeFalse)

				So(archiveFile.Close(), ShouldBeNil)
				So(os.RemoveAll(archiveFilePath), ShouldBeNil)
			})

			Convey("to a directory", func() {
				Convey("when the collection is excluded", func() {
					Convey("by --excludeCollections", func() {
						md.ToolOptions.DB = dbName
						md.OutputOptions.ExcludedCollections = []string{colName}
					})

					Convey("by --excludeCollectionsWithPrefix", func() {
						md.ToolOptions.DB = dbName
						md.OutputOptions.ExcludedCollectionPrefixes = []string{colName[:5]}
					})
				})

				err = md.Init()
				So(err, ShouldBeNil)

				err = md.Dump()
				So(err, ShouldBeNil)

				path, err := os.Getwd()
				So(err, ShouldBeNil)

				dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
				So(fileDirExists(dumpDir), ShouldBeFalse)

				So(os.RemoveAll(dumpDir), ShouldBeNil)
			})
		})

		Convey("specifying the buckets collection in --collection should fail", func() {
			md.ToolOptions.DB = dbName
			md.ToolOptions.Collection = "system.buckets." + colName

			err = md.Init()
			So(err, ShouldNotBeNil)
			So(
				err.Error(),
				ShouldEndWith,
				"cannot specify a system.buckets collection in --collection. "+
					"Specifying the timeseries collection will dump the system.buckets collection",
			)
		})

		Convey("querying the timeseries collection", func() {

			Convey("by a metadata field", func() {

				Convey("with the --query option", func() {
					md.ToolOptions.DB = dbName
					md.ToolOptions.Collection = colName
					md.InputOptions.Query = "{\"my_meta.device\": 1}"
				})

				Convey("with the --queryFile option", func() {
					err = os.WriteFile("ts_query.json", []byte("{\"my_meta.device\": 1}"), 0777)
					So(err, ShouldBeNil)
					md.ToolOptions.DB = dbName
					md.ToolOptions.Collection = colName
					md.InputOptions.QueryFile = "ts_query.json"
				})

				err = md.Init()
				So(err, ShouldBeNil)

				err = md.Dump()
				So(err, ShouldBeNil)

				path, err := os.Getwd()
				So(err, ShouldBeNil)

				dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
				dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, dbName))
				metadataFile := util.ToUniversalPath(
					filepath.Join(dumpDBDir, colName+".metadata.json"),
				)
				bsonFile := util.ToUniversalPath(
					filepath.Join(dumpDBDir, "system.buckets."+colName+".bson"),
				)
				So(fileDirExists(dumpDir), ShouldBeTrue)
				So(fileDirExists(dumpDBDir), ShouldBeTrue)
				So(fileDirExists(metadataFile), ShouldBeTrue)
				So(fileDirExists(bsonFile), ShouldBeTrue)

				allFiles, err := getMatchingFiles(dumpDBDir, ".*"+colName+".*")
				So(err, ShouldBeNil)
				So(len(allFiles), ShouldEqual, 2)

				info, err := os.Stat(bsonFile)
				So(err, ShouldBeNil)
				So(info.Size(), ShouldBeGreaterThan, 0)

				fd, err := os.Open(bsonFile)
				So(err, ShouldBeNil)

				bsonSource := db.NewBSONSource(fd)

				matchedDoc := bson.Raw(bsonSource.LoadNext())
				rawVal, err := matchedDoc.LookupErr("meta", "device")
				So(err, ShouldBeNil)

				val, ok := rawVal.Int32OK()
				So(ok, ShouldBeTrue)
				So(val, ShouldEqual, 1)

				// only one bucket document should be matched
				So(bsonSource.LoadNext(), ShouldBeNil)

				So(fd.Close(), ShouldBeNil)
				So(os.RemoveAll(dumpDir), ShouldBeNil)
				os.Remove("ts_query.json")

			})

		})

		Convey("by a non-metadata field", func() {

			Convey("with the --query option", func() {
				md.ToolOptions.DB = dbName
				md.ToolOptions.Collection = colName
				md.InputOptions.Query = "{\"wrong.device\": 1}"
			})

			Convey("with the --queryFile option", func() {
				err = os.WriteFile("ts_query.json", []byte("{\"wrong.device\": 1}"), 0777)
				So(err, ShouldBeNil)
				md.ToolOptions.DB = dbName
				md.ToolOptions.Collection = colName
				md.InputOptions.QueryFile = "ts_query.json"
			})

			err = md.Init()
			So(err, ShouldBeNil)

			err = md.Dump()
			So(err, ShouldNotBeNil)
			So(
				err.Error(),
				ShouldContainSubstring,
				"mongodump only processes queries on metadata fields for timeseries collections",
			)

			os.Remove("ts_query.json")

		})

	})

	err = dropDB(dbName)
	if err != nil {
		t.Logf("Failed to drop timeseries collection: %v", err)
	}
}

func TestDumpTimeseriesCollectionsWithMixedSchema(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}
	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "5.0"); err != nil || cmp < 0 {
		t.Skipf("Requires server with FCV 5.0 or later; found %v", fcv)
	}

	colName := "timeseries_mixed_schema"
	dbName := "timeseries_test_DB"

	require.NoError(t, setupTimeseriesWithMixedSchema(dbName, colName))

	md := simpleMongoDumpInstance()
	md.ToolOptions.Namespace.DB = dbName
	md.OutputOptions.Out = "dump"
	md.OutputOptions.Out = ""
	md.OutputOptions.Archive = "dump.archive"

	require.NoError(t, md.Init())

	require.NoError(t, md.Dump())

	path, err := os.Getwd()
	require.NoError(t, err)

	archiveFilePath := util.ToUniversalPath(filepath.Join(path, "dump.archive"))

	archiveFile, err := os.Open(archiveFilePath)
	require.NoError(t, err)
	archiveReader := &archive.Reader{
		In:      archiveFile,
		Prelude: &archive.Prelude{},
	}

	require.NoError(t, archiveReader.Prelude.Read(archiveReader.In))

	collectionMetadatas, ok := archiveReader.Prelude.NamespaceMetadatasByDB[dbName]
	require.True(t, ok)

	require.Len(t, collectionMetadatas, 1)
	require.Equal(t, colName, collectionMetadatas[0].Collection)

	pe, err := archiveReader.Prelude.NewPreludeExplorer()
	require.NoError(t, err)

	archiveContents, err := pe.ReadDir()
	require.NoError(t, err)

	for _, dirlike := range archiveContents {
		if dirlike.IsDir() && dirlike.Name() == dbName {
			dbContents, err := dirlike.ReadDir()
			require.NoError(t, err)

			require.Len(t, dbContents, 2)

			for _, file := range dbContents {
				require.Contains(
					t,
					[]string{colName + ".metadata.json", "system.buckets." + colName + ".bson"},
					file.Name(),
				)
			}
		}
	}

	require.NoError(t, archiveFile.Close())
	require.NoError(t, os.Remove(archiveFilePath))
	require.NoError(t, session.Database(dbName).Collection(colName).Drop(context.Background()))
}

func TestFailDuringResharding(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("Failed to get session provider: %v", err)
	}

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}

	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "4.9"); err != nil || cmp < 0 {
		t.Skipf("Requires server with FCV 4.9 or later; found %v", fcv)
	}

	ctx := context.Background()

	if ok, _ := sessionProvider.IsReplicaSet(); !ok {
		t.Skipf("Not for replica sets")
	}

	Convey("With a MongoDump instance", t, func() {
		err := setUpMongoDumpTestData()
		So(err, ShouldBeNil)

		md := simpleMongoDumpInstance()
		md.OutputOptions.Oplog = true
		md.ToolOptions.Namespace = &options.Namespace{}
		err = md.Init()
		So(err, ShouldBeNil)

		DefaultErrorMsg := "detected resharding in progress. Cannot dump with --oplog while resharding"
		OplogErrorMsg := "cannot dump with oplog while resharding"

		Convey("dump should fail if config.reshardingOperations exists on source", func() {
			err = session.Database("config").CreateCollection(ctx, "reshardingOperations")
			So(err, ShouldBeNil)
			//nolint:errcheck
			defer session.Database("config").Collection("reshardingOperations").Drop(ctx)

			err = md.Dump()
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, DefaultErrorMsg)
		})

		Convey(
			"dump should fail if config.localReshardingOperations.donor exists on source",
			func() {
				err = session.Database("config").
					CreateCollection(ctx, "localReshardingOperations.donor")
				So(err, ShouldBeNil)
				//nolint:errcheck
				defer session.Database("config").
					Collection("localReshardingOperations.donor").
					Drop(ctx)

				err = md.Dump()
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, DefaultErrorMsg)
			},
		)

		Convey(
			"dump should fail if config.localReshardingOperations.recipient exists on source",
			func() {
				err = session.Database("config").
					CreateCollection(ctx, "localReshardingOperations.recipient")
				So(err, ShouldBeNil)
				//nolint:errcheck
				defer session.Database("config").
					Collection("localReshardingOperations.recipient").
					Drop(ctx)

				err = md.Dump()
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, DefaultErrorMsg)
			},
		)

		Convey("dump should fail if config.reshardingOperations created in oplog", func() {
			failpoint.ParseFailpoints("PauseBeforeDumping")
			defer failpoint.Reset()

			var sessErr1, sessErr2 error
			go func() {
				time.Sleep(2 * time.Second)
				sessErr1 = session.Database("config").CreateCollection(ctx, "reshardingOperations")
				sessErr2 = session.Database("config").Collection("reshardingOperations").Drop(ctx)
			}()

			err = md.Dump()

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, OplogErrorMsg)
			So(sessErr1, ShouldBeNil)
			So(sessErr2, ShouldBeNil)
		})

		Convey(
			"dump should fail if config.localReshardingOperations.donor created in oplog",
			func() {
				failpoint.ParseFailpoints("PauseBeforeDumping")
				defer failpoint.Reset()

				var sessErr1, sessErr2 error
				go func() {
					time.Sleep(2 * time.Second)
					sessErr1 = session.Database("config").
						CreateCollection(ctx, "localReshardingOperations.donor")
					sessErr2 = session.Database("config").
						Collection("localReshardingOperations.donor").
						Drop(ctx)
				}()

				err = md.Dump()

				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, OplogErrorMsg)
				So(sessErr1, ShouldBeNil)
				So(sessErr2, ShouldBeNil)
			},
		)

		Convey(
			"dump should fail if config.localReshardingOperations.recipient created in oplog",
			func() {
				failpoint.ParseFailpoints("PauseBeforeDumping")
				defer failpoint.Reset()

				var sessErr1, sessErr2 error
				go func() {
					time.Sleep(2 * time.Second)
					sessErr1 = session.Database("config").
						CreateCollection(ctx, "localReshardingOperations.recipient")
					sessErr2 = session.Database("config").
						Collection("localReshardingOperations.recipient").
						Drop(ctx)
				}()

				err = md.Dump()

				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, OplogErrorMsg)
				So(sessErr1, ShouldBeNil)
				So(sessErr2, ShouldBeNil)
			},
		)

	})
}

func TestOptionsOrderIsPreserved(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	log.SetWriter(io.Discard)

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	require.NoError(t, err)

	collName := "students"
	viewName := "studentsView"

	pipeline := bson.A{
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "year", Value: "$year"},
				{Key: "name", Value: "$name"},
			}},
			{Key: "highest", Value: bson.D{
				{Key: "$max", Value: "$score"},
			}},
		}}},
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{
			{Key: "year", Value: 1},
			{Key: "sID", Value: -1},
			{Key: "name", Value: 1},
			{Key: "score", Value: 1},
		}}},
	}

	createViewCmd := bson.D{
		{"create", viewName},
		{"viewOn", collName},
		{"pipeline", pipeline},
	}

	var result bson.D
	err = sessionProvider.Run(createViewCmd, &result, testDB)
	require.NoError(t, err)

	// The check should be run a few times due to the probabilistic nature
	// of TOOLS-3411
	for i := 0; i < 10; i++ {
		dumpAndCheckPipelineOrder(t, collName, pipeline)
	}

	err = tearDownMongoDumpTestData()
	require.NoError(t, err)
}

func dumpAndCheckPipelineOrder(t *testing.T, collName string, pipeline bson.A) {
	md := simpleMongoDumpInstance()

	md.ToolOptions.Namespace.DB = testDB
	md.OutputOptions.Out = "dump"

	require.NoError(t, md.Init())
	require.NoError(t, md.Dump())

	path, err := os.Getwd()
	require.NoError(t, err)

	dumpDir := util.ToUniversalPath(filepath.Join(path, "dump"))
	dumpDBDir := util.ToUniversalPath(filepath.Join(dumpDir, testDB))
	require.True(t, fileDirExists(dumpDir))
	require.True(t, fileDirExists(dumpDBDir))

	metaFiles, err := getMatchingFiles(dumpDBDir, "studentsView\\.metadata\\.json")
	require.NoError(t, err)
	require.Equal(t, len(metaFiles), 1)

	metaFile, err := os.Open(util.ToUniversalPath(filepath.Join(dumpDBDir, metaFiles[0])))

	require.NoError(t, err)
	contents, err := io.ReadAll(metaFile)
	require.NoError(t, err)

	var bsonResult bson.D
	err = bson.UnmarshalExtJSON(contents, true, &bsonResult)
	require.NoError(t, err)
	options, err := bsonutil.FindSubdocumentByKey("options", &bsonResult)
	require.NoError(t, err)

	assertBSONEqual(t, options, bson.D{
		{Key: "viewOn", Value: collName},
		{Key: "pipeline", Value: pipeline},
	})

	os.RemoveAll(dumpDir)
	metaFile.Close()
}
