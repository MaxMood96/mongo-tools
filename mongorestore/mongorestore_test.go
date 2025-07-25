// Copyright (C) MongoDB, Inc. 2014-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package mongorestore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mongodb/mongo-tools/common/archive"
	"github.com/mongodb/mongo-tools/common/bsonutil"
	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/idx"
	"github.com/mongodb/mongo-tools/common/log"
	"github.com/mongodb/mongo-tools/common/options"
	"github.com/mongodb/mongo-tools/common/testtype"
	"github.com/mongodb/mongo-tools/common/testutil"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	mopt "go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"golang.org/x/sync/errgroup"
)

const (
	mioSoeFile     = "testdata/10k1dup10k.bson"
	longFilePrefix = "aVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVery" +
		"VeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVery" +
		"VeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVeryVery"
	longCollectionName = longFilePrefix +
		"LongCollectionNameConsistingOfExactlyTwoHundredAndFortySevenCharacters"
	longBsonName = longFilePrefix +
		"LongCollectionNameConsistingOfE%24xFO0VquRn7cg3QooSZD5sglTddU.bson"
	longMetadataName = longFilePrefix +
		"LongCollectionNameConsistingOfE%24xFO0VquRn7cg3QooSZD5sglTddU.metadata.json"
	longInvalidBson = longFilePrefix +
		"LongCollectionNameConsistingOfE%24someMadeUpInvalidHashString.bson"
	specialCharactersCollectionName = "cafés"
)

var testDocument = bson.M{"key": "value"}

var configCollectionNamesToKeep = []string{
	"chunks",
	"collections",
	"databases",
	"settings",
	"shards",
	"tags",
	"version",
}

var userDefinedConfigCollectionNames = []string{
	"coll1",
	"coll2",
	"coll3",
}

func init() {
	// bump up the verbosity to make checking debug log output possible
	log.SetVerbosity(&options.Verbosity{
		VLevel: 4,
	})
}

func getRestoreWithArgs(additionalArgs ...string) (*MongoRestore, error) {
	opts, err := ParseOptions(append(testutil.GetBareArgs(), additionalArgs...), "", "")
	if err != nil {
		return nil, fmt.Errorf("error parsing args: %v", err)
	}

	restore, err := New(opts)
	if err != nil {
		return nil, fmt.Errorf("error making new instance of mongorestore: %v", err)
	}

	return restore, nil
}

func TestDeprecatedDBAndCollectionOptions(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	// As specified in TOOLS-2363, the --db and --collection options
	// are well-defined only for restoration of a single BSON file
	Convey("The proper warning message is issued if --db and --collection "+
		"are used in a case where they are deprecated", t, func() {
		// Hacky way of looking at the application log at test-time

		// Ideally, we would be able to use some form of explicit dependency
		// injection to specify the sink for the parsing/validation log. However,
		// the validation logic here is coupled with the mongorestore.MongoRestore
		// type, which does not support such an injection.

		var buffer bytes.Buffer

		log.SetWriter(&buffer)
		defer log.SetWriter(os.Stderr)

		Convey("and no warning is issued in the well-defined case", func() {
			// No error and nothing written in the log
			args := []string{
				"testdata/hashedIndexes.bson",
				DBOption, "db1	",
				CollectionOption, "coll1",
			}

			restore, err := getRestoreWithArgs(args...)
			if err != nil {
				t.Fatalf("Cannot bootstrap test harness: %v", err.Error())
			}
			defer restore.Close()

			err = restore.ParseAndValidateOptions()

			So(err, ShouldBeNil)
			So(buffer.String(), ShouldBeEmpty)
		})

		Convey("and a warning is issued in the deprecated case", func() {
			// No error and some kind of warning message in the log
			args := []string{
				DBOption, "db1",
				CollectionOption, "coll1",
			}

			restore, err := getRestoreWithArgs(args...)
			if err != nil {
				t.Fatalf("Cannot bootstrap test harness: %v", err.Error())
			}
			defer restore.Close()

			err = restore.ParseAndValidateOptions()

			So(err, ShouldBeNil)
			So(buffer.String(), ShouldContainSubstring, deprecatedDBAndCollectionsOptionsWarning)
		})
	})
}

func TestMongorestore(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	Convey("With a test MongoRestore", t, func() {
		args := []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		db := session.Database("db1")
		Convey("and majority is used as the default write concern", func() {
			So(db.WriteConcern(), ShouldResemble, writeconcern.New(writeconcern.WMajority()))
		})

		c1 := db.Collection("c1") // 100 documents
		err = c1.Drop(context.Background())
		So(err, ShouldBeNil)
		c2 := db.Collection("c2") // 0 documents
		err = c2.Drop(context.Background())
		So(err, ShouldBeNil)
		c3 := db.Collection("c3") // 0 documents
		err = c3.Drop(context.Background())
		So(err, ShouldBeNil)
		c4 := db.Collection("c4") // 10 documents
		err = c4.Drop(context.Background())
		So(err, ShouldBeNil)

		Convey("and an explicit target restores from that dump directory", func() {
			restore.TargetDirectory = "testdata/testdirs"

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 110)
			So(result.Failures, ShouldEqual, 0)

			count, err := c1.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 100)

			count, err = c4.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 10)
		})

		Convey("and an target of '-' restores from standard input", func() {
			bsonFile, err := os.Open("testdata/testdirs/db1/c1.bson")
			So(err, ShouldBeNil)

			restore.ToolOptions.Namespace.Collection = "c1"
			restore.ToolOptions.Namespace.DB = "db1"
			restore.InputReader = bsonFile
			restore.TargetDirectory = "-"

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			count, err := c1.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 100)
		})

		Convey("and specifying an nsExclude option", func() {
			restore.TargetDirectory = "testdata/testdirs"
			restore.NSOptions.NSExclude = make([]string, 1)
			restore.NSOptions.NSExclude[0] = "db1.c1"

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 10)
			So(result.Failures, ShouldEqual, 0)

			count, err := c1.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 0)

			count, err = c4.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 10)
		})

		Convey("and specifying an nsInclude option", func() {
			restore.TargetDirectory = "testdata/testdirs"
			restore.NSOptions.NSInclude = make([]string, 1)
			restore.NSOptions.NSInclude[0] = "db1.c4"

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 10)
			So(result.Failures, ShouldEqual, 0)

			count, err := c1.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 0)

			count, err = c4.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 10)
		})

		Convey("and specifying nsFrom and nsTo options", func() {
			restore.TargetDirectory = "testdata/testdirs"

			restore.NSOptions.NSFrom = make([]string, 1)
			restore.NSOptions.NSFrom[0] = "db1.c1"
			restore.NSOptions.NSTo = make([]string, 1)
			restore.NSOptions.NSTo[0] = "db1.c1renamed"

			c1renamed := db.Collection("c1renamed")
			err = c1renamed.Drop(context.Background())
			So(err, ShouldBeNil)

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 110)
			So(result.Failures, ShouldEqual, 0)

			count, err := c1renamed.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 100)

			count, err = c4.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 10)
		})
	})
}

func TestMongoRestoreSpecialCharactersCollectionNames(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	Convey("With a test MongoRestore", t, func() {
		args := []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		db := session.Database("db1")

		specialCharacterCollection := db.Collection(specialCharactersCollectionName)
		err = specialCharacterCollection.Drop(context.Background())
		So(err, ShouldBeNil)

		Convey("and --nsInclude a collection name with special characters", func() {
			restore.TargetDirectory = "testdata/specialcharacter"
			restore.NSOptions.NSInclude = make([]string, 1)
			restore.NSOptions.NSInclude[0] = "db1." + specialCharactersCollectionName

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 1)
			So(result.Failures, ShouldEqual, 0)

			count, err := specialCharacterCollection.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 1)
		})

		Convey("and --nsExclude a collection name with special characters", func() {
			restore.TargetDirectory = "testdata/specialcharacter"
			restore.NSOptions.NSExclude = make([]string, 1)
			restore.NSOptions.NSExclude[0] = "db1." + specialCharactersCollectionName
			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 0)
			So(result.Failures, ShouldEqual, 0)

			count, err := specialCharacterCollection.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 0)
		})

		Convey("and --nsTo a collection name without special characters "+
			"--nsFrom a collection name with special characters", func() {
			restore.TargetDirectory = "testdata/specialcharacter"
			restore.NSOptions.NSFrom = make([]string, 1)
			restore.NSOptions.NSFrom[0] = "db1." + specialCharactersCollectionName
			restore.NSOptions.NSTo = make([]string, 1)
			restore.NSOptions.NSTo[0] = "db1.aCollectionNameWithoutSpecialCharacters"

			standardCharactersCollection := db.Collection("aCollectionNameWithoutSpecialCharacters")
			err = standardCharactersCollection.Drop(context.Background())
			So(err, ShouldBeNil)

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 1)
			So(result.Failures, ShouldEqual, 0)

			count, err := standardCharactersCollection.CountDocuments(
				context.Background(),
				bson.M{},
			)
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 1)
		})

		Convey("and --nsTo a collection name with special characters "+
			"--nsFrom a collection name with special characters", func() {
			restore.TargetDirectory = "testdata/specialcharacter"
			restore.NSOptions.NSFrom = make([]string, 1)
			restore.NSOptions.NSFrom[0] = "db1." + specialCharactersCollectionName
			restore.NSOptions.NSTo = make([]string, 1)
			restore.NSOptions.NSTo[0] = "db1.aCollectionNameWithSpećiálCharacters"

			standardCharactersCollection := db.Collection("aCollectionNameWithSpećiálCharacters")
			err = standardCharactersCollection.Drop(context.Background())
			So(err, ShouldBeNil)

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 1)
			So(result.Failures, ShouldEqual, 0)

			count, err := standardCharactersCollection.CountDocuments(
				context.Background(),
				bson.M{},
			)
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 1)
		})
	})
}

func TestMongorestoreLongCollectionName(t *testing.T) {
	// Disabled: see TOOLS-2658
	t.Skip()

	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}
	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "4.4"); err != nil || cmp < 0 {
		t.Skip("Requires server with FCV 4.4 or later")
	}

	Convey("With a test MongoRestore", t, func() {
		args := []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		db := session.Database("db1")
		Convey("and majority is used as the default write concern", func() {
			So(db.WriteConcern(), ShouldResemble, writeconcern.New(writeconcern.WMajority()))
		})

		longCollection := db.Collection(longCollectionName)
		err = longCollection.Drop(context.Background())
		So(err, ShouldBeNil)

		Convey("and an explicit target restores truncated files from that dump directory", func() {
			restore.TargetDirectory = "testdata/longcollectionname"

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 1)
			So(result.Failures, ShouldEqual, 0)

			count, err := longCollection.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 1)
		})

		Convey("and an target of '-' restores truncated files from standard input", func() {
			longBsonFile, err := os.Open("testdata/longcollectionname/db1/" + longBsonName)
			So(err, ShouldBeNil)

			restore.ToolOptions.Namespace.Collection = longCollectionName
			restore.ToolOptions.Namespace.DB = "db1"
			restore.InputReader = longBsonFile
			restore.TargetDirectory = "-"
			result := restore.Restore()
			So(result.Err, ShouldBeNil)

			count, err := longCollection.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 1)
		})

		Convey("and specifying an nsExclude option", func() {
			restore.TargetDirectory = "testdata/longcollectionname"
			restore.NSOptions.NSExclude = make([]string, 1)
			restore.NSOptions.NSExclude[0] = "db1." + longCollectionName

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 0)
			So(result.Failures, ShouldEqual, 0)

			count, err := longCollection.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 0)
		})

		Convey("and specifying an nsInclude option", func() {
			restore.TargetDirectory = "testdata/longcollectionname"
			restore.NSOptions.NSInclude = make([]string, 1)
			restore.NSOptions.NSInclude[0] = "db1." + longCollectionName

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 1)
			So(result.Failures, ShouldEqual, 0)

			count, err := longCollection.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 1)
		})

		Convey("and specifying nsFrom and nsTo options", func() {
			restore.TargetDirectory = "testdata/longcollectionname"
			restore.NSOptions.NSFrom = make([]string, 1)
			restore.NSOptions.NSFrom[0] = "db1." + longCollectionName
			restore.NSOptions.NSTo = make([]string, 1)
			restore.NSOptions.NSTo[0] = "db1.aMuchShorterCollectionName"

			shortCollection := db.Collection("aMuchShorterCollectionName")
			err = shortCollection.Drop(context.Background())
			So(err, ShouldBeNil)

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 1)
			So(result.Failures, ShouldEqual, 0)

			count, err := shortCollection.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 1)
		})
	})
}

func TestMongorestorePreserveUUID(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}
	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "3.6"); err != nil || cmp < 0 {
		t.Skip("Requires server with FCV 3.6 or later")
	}

	// From mongorestore/testdata/oplogdump/db1/c1.metadata.json
	originalUUID := "699f503df64b4aa8a484a8052046fa3a"

	Convey("With a test MongoRestore", t, func() {
		c1 := session.Database("db1").Collection("c1")
		err = c1.Drop(context.Background())
		So(err, ShouldBeNil)

		Convey("normal restore gives new UUID", func() {
			args := []string{
				NumParallelCollectionsOption, "1",
				NumInsertionWorkersOption, "1",
				"testdata/oplogdump",
			}
			restore, err := getRestoreWithArgs(args...)
			So(err, ShouldBeNil)
			defer restore.Close()

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			count, err := c1.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 5)
			info, err := db.GetCollectionInfo(c1)
			So(err, ShouldBeNil)
			So(info.GetUUID(), ShouldNotEqual, originalUUID)
		})

		Convey("PreserveUUID restore without drop errors", func() {
			args := []string{
				NumParallelCollectionsOption, "1",
				NumInsertionWorkersOption, "1",
				PreserveUUIDOption,
				"testdata/oplogdump",
			}
			restore, err := getRestoreWithArgs(args...)
			So(err, ShouldBeNil)
			defer restore.Close()

			result := restore.Restore()
			So(result.Err, ShouldNotBeNil)
			So(
				result.Err.Error(),
				ShouldContainSubstring,
				"cannot specify --preserveUUID without --drop",
			)
		})

		Convey("PreserveUUID with drop preserves UUID", func() {
			args := []string{
				NumParallelCollectionsOption, "1",
				NumInsertionWorkersOption, "1",
				PreserveUUIDOption,
				DropOption,
				"testdata/oplogdump",
			}
			restore, err := getRestoreWithArgs(args...)
			So(err, ShouldBeNil)
			defer restore.Close()

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			count, err := c1.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 5)
			info, err := db.GetCollectionInfo(c1)
			So(err, ShouldBeNil)
			So(info.GetUUID(), ShouldEqual, originalUUID)
		})

		Convey("PreserveUUID on a file without UUID metadata errors", func() {
			args := []string{
				NumParallelCollectionsOption, "1",
				NumInsertionWorkersOption, "1",
				PreserveUUIDOption,
				DropOption,
				"testdata/testdirs",
			}
			restore, err := getRestoreWithArgs(args...)
			So(err, ShouldBeNil)
			defer restore.Close()

			result := restore.Restore()
			So(result.Err, ShouldBeNil)
		})

	})
}

// generateTestData creates the files used in TestMongorestoreMIOSOE.
func generateTestData() error {
	// If file exists already, don't both regenerating it.
	if _, err := os.Stat(mioSoeFile); err == nil {
		return nil
	}

	f, err := os.Create(mioSoeFile)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)

	// 10k unique _id's
	for i := 1; i < 10001; i++ {
		buf, err := bson.Marshal(bson.D{{"_id", i}})
		if err != nil {
			return err
		}
		_, err = w.Write(buf)
		if err != nil {
			return err
		}
	}

	// 1 duplicate _id
	buf, err := bson.Marshal(bson.D{{"_id", 5}})
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	if err != nil {
		return err
	}

	// 10k unique _id's
	for i := 10001; i < 20001; i++ {
		buf, err := bson.Marshal(bson.D{{"_id", i}})
		if err != nil {
			return err
		}
		_, err = w.Write(buf)
		if err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	return nil
}

// test --maintainInsertionOrder and --stopOnError behavior.
func TestMongorestoreMIOSOE(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	if err := generateTestData(); err != nil {
		t.Fatalf("Couldn't generate test data %v", err)
	}

	client, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}
	database := client.Database("miodb")
	coll := database.Collection("mio")

	Convey("default restore ignores dup key errors", t, func() {
		restore, err := getRestoreWithArgs(mioSoeFile,
			CollectionOption, coll.Name(),
			DBOption, database.Name(),
			DropOption)
		So(err, ShouldBeNil)
		defer restore.Close()
		So(restore.OutputOptions.MaintainInsertionOrder, ShouldBeFalse)

		result := restore.Restore()
		So(result.Err, ShouldBeNil)
		So(result.Successes, ShouldEqual, 20000)
		So(result.Failures, ShouldEqual, 1)

		count, err := coll.CountDocuments(context.Background(), bson.M{})
		So(err, ShouldBeNil)
		So(count, ShouldEqual, 20000)
	})

	Convey("--maintainInsertionOrder stops exactly on dup key errors", t, func() {
		restore, err := getRestoreWithArgs(mioSoeFile,
			CollectionOption, coll.Name(),
			DBOption, database.Name(),
			DropOption,
			MaintainInsertionOrderOption)
		So(err, ShouldBeNil)
		defer restore.Close()
		So(restore.OutputOptions.MaintainInsertionOrder, ShouldBeTrue)
		So(restore.OutputOptions.NumInsertionWorkers, ShouldEqual, 1)

		result := restore.Restore()
		So(result.Err, ShouldNotBeNil)
		So(result.Successes, ShouldEqual, 10000)
		So(result.Failures, ShouldEqual, 1)

		count, err := coll.CountDocuments(context.Background(), bson.M{})
		So(err, ShouldBeNil)
		So(count, ShouldEqual, 10000)
	})

	Convey("--stopOnError stops on dup key errors", t, func() {
		restore, err := getRestoreWithArgs(mioSoeFile,
			CollectionOption, coll.Name(),
			DBOption, database.Name(),
			DropOption,
			StopOnErrorOption,
			NumParallelCollectionsOption, "1")
		So(err, ShouldBeNil)
		defer restore.Close()
		So(restore.OutputOptions.StopOnError, ShouldBeTrue)

		result := restore.Restore()
		So(result.Err, ShouldNotBeNil)
		So(result.Successes, ShouldAlmostEqual, 10000, restore.OutputOptions.BulkBufferSize)
		So(result.Failures, ShouldEqual, 1)

		count, err := coll.CountDocuments(context.Background(), bson.M{})
		So(err, ShouldBeNil)
		So(count, ShouldAlmostEqual, 10000, restore.OutputOptions.BulkBufferSize)
	})

	err = database.Drop(context.Background())
	if err != nil {
		t.Fatalf("Could not drop database")
	}
}

func TestDeprecatedIndexOptions(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	Convey("With a test MongoRestore", t, func() {
		args := []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		db := session.Database("indextest")

		coll := db.Collection("test_collection")
		err = coll.Drop(context.Background())
		So(err, ShouldBeNil)
		defer func() {
			dropErr := coll.Drop(context.Background())
			So(dropErr, ShouldBeNil)
		}()
		Convey("Creating index with invalid option should throw error", func() {
			restore.TargetDirectory = "testdata/indextestdump"
			result := restore.Restore()
			So(result.Err, ShouldNotBeNil)
			So(
				result.Err.Error(),
				ShouldStartWith,
				`indextest.test_collection: error creating indexes for indextest.test_collection: createIndex error:`,
			)

			So(result.Successes, ShouldEqual, 100)
			So(result.Failures, ShouldEqual, 0)
			count, err := coll.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 100)
		})

		err = coll.Drop(context.Background())
		So(err, ShouldBeNil)

		args = []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
			ConvertLegacyIndexesOption, "true",
		}

		restore, err = getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		Convey(
			"Creating index with invalid option and --convertLegacyIndexes should succeed",
			func() {
				restore.TargetDirectory = "testdata/indextestdump"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				So(result.Successes, ShouldEqual, 100)
				So(result.Failures, ShouldEqual, 0)
				count, err := coll.CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 100)
			},
		)
	})
}

// TestFixDuplicatedLegacyIndexes restores two indexes with --convertLegacyIndexes flag, {foo: ""} and {foo: 1}
// Only one index {foo: 1} should be created.
func TestFixDuplicatedLegacyIndexes(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "3.4"); err != nil || cmp < 0 {
		t.Skip("Requires server with FCV 3.4 or later")
	}
	Convey("With a test MongoRestore", t, func() {
		args := []string{
			ConvertLegacyIndexesOption,
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		Convey("Index with duplicate key after convertLegacyIndexes should be skipped", func() {
			restore.TargetDirectory = "testdata/duplicate_index_key"
			result := restore.Restore()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 0)
			So(result.Failures, ShouldEqual, 0)
			So(err, ShouldBeNil)

			testDB := session.Database("indextest")
			defer func() {
				err = testDB.Drop(context.Background())
				if err != nil {
					t.Fatalf("Failed to drop test database testdata")
				}
			}()

			c, err := testDB.Collection("duplicate_index_key").Indexes().List(context.Background())
			So(err, ShouldBeNil)

			type indexRes struct {
				Name string
				Key  bson.D
			}

			indexKeys := make(map[string]bson.D)

			// two Indexes should be created in addition to the _id, foo and foo_2
			for c.Next(context.Background()) {
				var res indexRes
				err = c.Decode(&res)
				So(err, ShouldBeNil)
				So(len(res.Key), ShouldEqual, 1)
				indexKeys[res.Name] = res.Key
			}

			So(len(indexKeys), ShouldEqual, 3)

			var indexKey bson.D
			// Check that only one of foo_, foo_1, or foo_1.0 was created
			indexKeyFoo, ok := indexKeys["foo_"]
			indexKeyFoo1, ok1 := indexKeys["foo_1"]
			indexKeyFoo10, ok10 := indexKeys["foo_1.0"]

			So(ok || ok1 || ok10, ShouldBeTrue)

			if ok {
				So(ok1 || ok10, ShouldBeFalse)
				indexKey = indexKeyFoo
			}

			if ok1 {
				So(ok || ok10, ShouldBeFalse)
				indexKey = indexKeyFoo1
			}

			if ok10 {
				So(ok || ok1, ShouldBeFalse)
				indexKey = indexKeyFoo10
			}

			So(len(indexKey), ShouldEqual, 1)
			So(indexKey[0].Key, ShouldEqual, "foo")
			So(indexKey[0].Value, ShouldEqual, 1)

			indexKey, ok = indexKeys["foo_2"]
			So(ok, ShouldBeTrue)
			So(len(indexKey), ShouldEqual, 1)
			So(indexKey[0].Key, ShouldEqual, "foo")
			So(indexKey[0].Value, ShouldEqual, 2)
		})
	})
}

func TestDeprecatedIndexOptionsOn44FCV(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}
	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "4.4"); err != nil || cmp < 0 {
		t.Skip("Requires server with FCV 4.4 or later")
	}

	Convey("With a test MongoRestore", t, func() {
		args := []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		session, _ = restore.SessionProvider.GetSession()

		db := session.Database("indextest")

		// 4.4 removes the 'ns' field nested under the 'index' field in metadata.json
		coll := db.Collection("test_coll_no_index_ns")
		err = coll.Drop(context.Background())
		So(err, ShouldBeNil)
		defer func() {
			dropErr := coll.Drop(context.Background())
			So(dropErr, ShouldBeNil)
		}()

		args = []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
			ConvertLegacyIndexesOption, "true",
		}

		restore, err = getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		Convey("Creating index with --convertLegacyIndexes and 4.4 FCV should succeed", func() {
			restore.TargetDirectory = "testdata/indexmetadata"
			result := restore.Restore()
			So(result.Err, ShouldBeNil)

			So(result.Successes, ShouldEqual, 100)
			So(result.Failures, ShouldEqual, 0)
			count, err := coll.CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 100)
		})
	})
}

func TestLongIndexName(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	Convey("With a test MongoRestore", t, func() {
		args := []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		session, err := restore.SessionProvider.GetSession()
		So(err, ShouldBeNil)

		coll := session.Database("longindextest").Collection("test_collection")
		err = coll.Drop(context.Background())
		So(err, ShouldBeNil)
		defer func() {
			dropErr := coll.Drop(context.Background())
			So(dropErr, ShouldBeNil)
		}()

		if restore.serverVersion.LT(db.Version{4, 2, 0}) {
			Convey(
				"Creating index with a full name longer than 127 bytes should fail (<4.2)",
				func() {
					restore.TargetDirectory = "testdata/longindextestdump"
					result := restore.Restore()
					So(result.Err, ShouldNotBeNil)
					So(
						result.Err.Error(),
						ShouldContainSubstring,
						"namespace is too long (max size is 127 bytes)",
					)
				},
			)
		} else {
			Convey("Creating index with a full name longer than 127 bytes should succeed (>=4.2)", func() {
				restore.TargetDirectory = "testdata/longindextestdump"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				indexes := session.Database("longindextest").Collection("test_collection").Indexes()
				c, err := indexes.List(context.Background())
				So(err, ShouldBeNil)

				type indexRes struct {
					Name string
				}
				var names []string
				for c.Next(context.Background()) {
					var r indexRes
					err := c.Decode(&r)
					So(err, ShouldBeNil)
					names = append(names, r.Name)
				}
				So(len(names), ShouldEqual, 2)
				sort.Strings(names)
				So(names[0], ShouldEqual, "_id_")
				So(names[1], ShouldEqual, "a_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
			})
		}

	})
}

func TestRestoreUsersOrRoles(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	Convey("With a test MongoRestore", t, func() {
		args := []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
		}

		Convey("Restoring users and roles should drop tempusers and temproles", func() {

			restore, err := getRestoreWithArgs(args...)
			So(err, ShouldBeNil)
			defer restore.Close()

			db := session.Database("admin")

			restore.TargetDirectory = "testdata/usersdump"
			result := restore.Restore()
			So(result.Err, ShouldBeNil)

			adminCollections, err := db.ListCollectionNames(context.Background(), bson.M{})
			So(err, ShouldBeNil)

			for _, collName := range adminCollections {
				So(collName, ShouldNotEqual, "tempusers")
				So(collName, ShouldNotEqual, "temproles")
			}
		})

		Convey("If --dumpUsersAndRoles was not used with the target", func() {
			Convey("Restoring from db directory should not be allowed", func() {
				args = append(
					args,
					RestoreDBUsersAndRolesOption,
					DBOption,
					"db1",
					"testdata/testdirs/db1",
				)

				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)
				defer restore.Close()

				result := restore.Restore()
				So(errors.Is(result.Err, NoUsersOrRolesInDumpError), ShouldBeTrue)
			})

			Convey("Restoring from base dump directory should not be allowed", func() {
				args = append(
					args,
					RestoreDBUsersAndRolesOption,
					DBOption,
					"db1",
					"testdata/testdirs",
				)

				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)
				defer restore.Close()

				result := restore.Restore()
				So(errors.Is(result.Err, NoUsersOrRolesInDumpError), ShouldBeTrue)
			})

			Convey("Restoring from archive of entire dump should not be allowed", func() {
				withArchiveMongodump(t, func(archive string) {
					args = append(
						args,
						RestoreDBUsersAndRolesOption,
						DBOption,
						"db1",
						ArchiveOption+"="+archive,
					)

					restore, err := getRestoreWithArgs(args...)
					So(err, ShouldBeNil)
					defer restore.Close()

					result := restore.Restore()
					So(errors.Is(result.Err, NoUsersOrRolesInDumpError), ShouldBeTrue)

				})
			})
		})
	})
}

func TestKnownCollections(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	Convey("With a test MongoRestore", t, func() {
		args := []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		session, _ = restore.SessionProvider.GetSession()
		db := session.Database("test")
		defer func() {
			dropErr := db.Collection("foo").Drop(context.Background())
			So(dropErr, ShouldBeNil)
		}()

		Convey(
			"Once collection foo has been restored, it should exist in restore.knownCollections",
			func() {
				restore.TargetDirectory = "testdata/foodump"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				var namespaceExistsInCache bool
				if cols, ok := restore.knownCollections["test"]; ok {
					for _, collName := range cols {
						if collName == "foo" {
							namespaceExistsInCache = true
						}
					}
				}
				So(namespaceExistsInCache, ShouldBeTrue)
			},
		)
	})
}

func TestReadPreludeMetadata(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	Convey("With a test MongoRestore", t, func() {
		args := []string{
			NumParallelCollectionsOption, "1",
			NumInsertionWorkersOption, "1",
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		session, _ = restore.SessionProvider.GetSession()
		database := session.Database("test")
		defer func() {
			dropErr := database.Collection("foo").Drop(context.Background())
			So(dropErr, ShouldBeNil)
		}()

		Convey("sets serverDumpVersion from prelude.json when dump dir is target", func() {
			restore.TargetDirectory = "testdata/prelude_test/prelude_top_level"
			result := restore.Restore()
			So(result.Err, ShouldBeNil)

			So(restore.dumpServerVersion, ShouldEqual, db.Version{7, 0, 16})
		})

		Convey("sets serverDumpVersion from prelude.json.gz when gzipped dump is used", func() {
			restore.TargetDirectory = "testdata/prelude_test/prelude_gzip/test"
			restore.InputOptions.Gzip = true
			result := restore.Restore()
			So(result.Err, ShouldBeNil)

			So(restore.dumpServerVersion, ShouldEqual, db.Version{7, 0, 16})
		})

		Convey(
			"sets serverDumpVersion from prelude.json in main dump dir when db dir is target",
			func() {
				restore.TargetDirectory = "testdata/prelude_test/prelude_top_level/test"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				So(restore.dumpServerVersion, ShouldEqual, db.Version{7, 0, 16})
			},
		)

		Convey(
			"sets serverDumpVersion from prelude.json from the db's directory",
			func() {
				restore.TargetDirectory = "testdata/prelude_test/prelude_db_target/test"
				restore.ToolOptions.Namespace.DB = "test"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				So(restore.dumpServerVersion, ShouldEqual, db.Version{7, 0, 16})
			},
		)

		Convey(
			"sets serverDumpVersion from prelude.json in parent directory when file is used as target",
			func() {
				restore.TargetDirectory = "testdata/prelude_test/prelude_top_level/test/foo.bson"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				So(restore.dumpServerVersion, ShouldEqual, db.Version{7, 0, 16})
			},
		)

		Convey(
			"does not error out when server version is unknown",
			func() {
				restore.TargetDirectory = "testdata/prelude_test/server_version_unknown"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				So(restore.dumpServerVersion, ShouldEqual, db.Version{})
			},
		)

		Convey(
			"does not error out when prelude is not available",
			func() {
				restore.TargetDirectory = "testdata/foodump"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				So(restore.dumpServerVersion, ShouldEqual, db.Version{})
			},
		)
	})
}

func TestFixHashedIndexes(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	type indexRes struct {
		Key bson.D
	}

	Convey("Test MongoRestore with hashed indexes and --fixHashedIndexes", t, func() {
		args := []string{
			FixDottedHashedIndexesOption,
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		db := session.Database("testdata")

		defer func() {
			dropErr := db.Collection("hashedIndexes").Drop(context.Background())
			So(dropErr, ShouldBeNil)
		}()

		Convey(
			"The index for a.b should be changed from 'hashed' to 1, since it is dotted",
			func() {
				restore.TargetDirectory = "testdata/hashedIndexes.bson"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				indexes := db.Collection("hashedIndexes").Indexes()
				c, err := indexes.List(context.Background())
				So(err, ShouldBeNil)
				var res indexRes

				for c.Next(context.Background()) {
					err := c.Decode(&res)
					So(err, ShouldBeNil)
					for _, key := range res.Key {
						if key.Key == "b" {
							So(key.Value, ShouldEqual, "hashed")
						} else if key.Key == "a.a" {
							So(key.Value, ShouldEqual, 1)
						} else if key.Key == "a.b" {
							So(key.Value, ShouldEqual, 1)
						} else if key.Key != "_id" {
							t.Fatalf("Unexepected Index: %v", key.Key)
						}
					}
				}
			},
		)
	})

	Convey("Test MongoRestore with hashed indexes without --fixHashedIndexes", t, func() {
		args := []string{}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		db := session.Database("testdata")

		defer func() {
			dropErr := db.Collection("hashedIndexes").Drop(context.Background())
			So(dropErr, ShouldBeNil)
		}()

		Convey("All indexes should be unchanged", func() {
			restore.TargetDirectory = "testdata/hashedIndexes.bson"
			result := restore.Restore()
			So(result.Err, ShouldBeNil)

			indexes := db.Collection("hashedIndexes").Indexes()
			c, err := indexes.List(context.Background())
			So(err, ShouldBeNil)
			var res indexRes

			for c.Next(context.Background()) {
				err := c.Decode(&res)
				So(err, ShouldBeNil)
				for _, key := range res.Key {
					if key.Key == "b" {
						So(key.Value, ShouldEqual, "hashed")
					} else if key.Key == "a.a" {
						So(key.Value, ShouldEqual, 1)
					} else if key.Key == "a.b" {
						So(key.Value, ShouldEqual, "hashed")
					} else if key.Key != "_id" {
						t.Fatalf("Unexepected Index: %v", key.Key)
					}
				}
			}
		})
	})
}

func TestAutoIndexIdLocalDB(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	ctx := context.Background()

	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	Convey(
		"Test MongoRestore with {autoIndexId: false} in a local database's collection",
		t,
		func() {
			dbName := session.Database("local")

			// Drop the collection to clean up resources
			//
			//nolint:errcheck
			defer dbName.Collection("test_auto_idx").Drop(ctx)

			opts, err := ParseOptions(testutil.GetBareArgs(), "", "")
			So(err, ShouldBeNil)

			// Set retryWrites to false since it is unsupported on `local` db.
			retryWrites := false
			opts.RetryWrites = &retryWrites

			restore, err := New(opts)
			So(err, ShouldBeNil)

			restore.TargetDirectory = "testdata/local/test_auto_idx.bson"
			result := restore.Restore()
			So(result.Err, ShouldBeNil)

			// Find the collection
			filter := bson.D{{"name", "test_auto_idx"}}
			cursor, err := session.Database("local").ListCollections(ctx, filter)
			So(err, ShouldBeNil)

			defer cursor.Close(ctx)

			documentExists := cursor.Next(ctx)
			So(documentExists, ShouldBeTrue)

			var collInfo struct {
				Options bson.M
			}
			err = cursor.Decode(&collInfo)
			So(err, ShouldBeNil)

			So(collInfo.Options["autoIndexId"], ShouldBeFalse)
		},
	)
}

func TestAutoIndexIdNonLocalDB(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	ctx := context.Background()

	session, err := testutil.GetBareSession()
	if err != nil {
		t.Fatalf("No server available")
	}

	Convey(
		"Test MongoRestore with {autoIndexId: false} in a non-local database's collection",
		t,
		func() {
			Convey("Do not set --preserveUUID\n", func() {
				dbName := session.Database("testdata")

				// Drop the collection to clean up resources
				//
				//nolint:errcheck
				defer dbName.Collection("test_auto_idx").Drop(ctx)

				var args []string

				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)
				defer restore.Close()

				restore.TargetDirectory = "testdata/test_auto_idx.bson"
				result := restore.Restore()
				So(result.Err, ShouldBeNil)

				// Find the collection
				filter := bson.D{{"name", "test_auto_idx"}}
				cursor, err := session.Database("testdata").ListCollections(ctx, filter)
				So(err, ShouldBeNil)

				defer cursor.Close(ctx)

				documentExists := cursor.Next(ctx)
				So(documentExists, ShouldBeTrue)

				var collInfo struct {
					Options bson.M
				}
				err = cursor.Decode(&collInfo)
				So(err, ShouldBeNil)

				Convey(
					"{autoIndexId: false} should be flipped to true if server version >= 4.0",
					func() {
						if restore.serverVersion.GTE(db.Version{4, 0, 0}) {
							So(collInfo.Options["autoIndexId"], ShouldBeTrue)
						} else {
							So(collInfo.Options["autoIndexId"], ShouldBeFalse)
						}
					},
				)
			})
			dbName := session.Database("testdata")

			// Drop the collection to clean up resources
			//
			//nolint:errcheck
			defer dbName.Collection("test_auto_idx").Drop(ctx)

			args := []string{
				PreserveUUIDOption, "1",
				DropOption,
			}

			restore, err := getRestoreWithArgs(args...)
			So(err, ShouldBeNil)
			defer restore.Close()

			if restore.serverVersion.GTE(db.Version{4, 0, 0}) {
				Convey("Set --preserveUUID if server version >= 4.0\n", func() {
					restore.TargetDirectory = "testdata/test_auto_idx.bson"
					result := restore.Restore()
					So(result.Err, ShouldBeNil)

					// Find the collection
					filter := bson.D{{"name", "test_auto_idx"}}
					cursor, err := session.Database("testdata").ListCollections(ctx, filter)
					So(err, ShouldBeNil)

					defer cursor.Close(ctx)

					documentExists := cursor.Next(ctx)
					So(documentExists, ShouldBeTrue)

					var collInfo struct {
						Options bson.M
					}
					err = cursor.Decode(&collInfo)
					So(err, ShouldBeNil)

					Convey(
						"{autoIndexId: false} should be flipped to true if server version >= 4.0",
						func() {
							if restore.serverVersion.GTE(db.Version{4, 0, 0}) {
								So(collInfo.Options["autoIndexId"], ShouldBeTrue)
							} else {
								So(collInfo.Options["autoIndexId"], ShouldBeFalse)
							}
						},
					)
				})
			}
		},
	)
}

// TestSkipSystemCollections asserts that certain system collections like "config.systems.sessions" and the transaction
// related tables aren't applied via applyops when replaying the oplog.
func TestSkipSystemCollections(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	ctx := context.Background()

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}
	defer sessionProvider.Close()

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("No client available")
	}

	if ok, _ := sessionProvider.IsReplicaSet(); !ok {
		t.SkipNow()
	}

	_, err = sessionProvider.GetNodeType()
	if err != nil {
		t.Fatalf("Could not get node type")
	}

	Convey("With a test MongoRestore instance", t, func() {
		db3 := session.Database("db3")

		// Drop the collection to clean up resources
		//
		//nolint:errcheck
		defer db3.Collection("c1").Drop(ctx)

		args := []string{
			DirectoryOption, "testdata/oplog_partial_skips",
			OplogReplayOption,
			DropOption,
		}

		currentTS := uint32(time.Now().UTC().Unix())

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		// Run mongorestore
		result := restore.Restore()
		So(result.Err, ShouldBeNil)

		Convey(
			"applyOps should skip certain system-related collections during mongorestore",
			func() {
				queryObj := bson.D{
					{"$and",
						bson.A{
							bson.D{{"ts", bson.M{"$gte": primitive.Timestamp{T: currentTS, I: 1}}}},
							bson.D{{"$or", bson.A{
								bson.D{
									{"ns", primitive.Regex{Pattern: "^config.system.sessions*"}},
								},
								bson.D{{"ns", primitive.Regex{Pattern: "^config.cache.*"}}},
							}}},
						},
					},
				}

				cursor, err := session.Database("local").
					Collection("oplog.rs").
					Find(context.Background(), queryObj, nil)
				So(err, ShouldBeNil)

				flag := cursor.Next(ctx)
				So(flag, ShouldBeFalse)

				cursor.Close(ctx)
			},
		)
	})
}

// TestSkipStartAndAbortIndexBuild asserts that all "startIndexBuild" and "abortIndexBuild" oplog
// entries are skipped when restoring the oplog.
func TestSkipStartAndAbortIndexBuild(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	ctx := context.Background()

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}
	defer sessionProvider.Close()

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("No client available")
	}

	if ok, _ := sessionProvider.IsReplicaSet(); !ok {
		t.SkipNow()
	}

	Convey("With a test MongoRestore instance", t, func() {
		testdb := session.Database("test")

		// Drop the collection to clean up resources
		//
		//nolint:errcheck
		defer testdb.Collection("skip_index_entries").Drop(ctx)

		// oplog.bson only has startIndexBuild and abortIndexBuild entries
		args := []string{
			DirectoryOption, "testdata/oplog_ignore_index",
			OplogReplayOption,
			DropOption,
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		if restore.serverVersion.GTE(db.Version{4, 4, 0}) {
			// Run mongorestore
			dbLocal := session.Database("local")
			queryObj := bson.D{{
				"and", bson.A{
					bson.D{{"ns", bson.M{"$ne": "config.system.sessions"}}},
					bson.D{{"op", bson.M{"$ne": "n"}}},
				},
			}}

			countBeforeRestore, err := dbLocal.Collection("oplog.rs").CountDocuments(ctx, queryObj)
			So(err, ShouldBeNil)

			result := restore.Restore()
			So(result.Err, ShouldBeNil)

			Convey("No new oplog entries should be recorded", func() {
				// Filter out no-ops
				countAfterRestore, err := dbLocal.Collection("oplog.rs").
					CountDocuments(ctx, queryObj)

				So(err, ShouldBeNil)
				So(countBeforeRestore, ShouldEqual, countAfterRestore)
			})
		}
	})
}

// TestcommitIndexBuild asserts that all "commitIndexBuild" are converted to creatIndexes commands.
func TestCommitIndexBuild(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	ctx := context.Background()
	testDB := "commit_index"

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}
	defer sessionProvider.Close()

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("No client available")
	}

	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "4.4"); err != nil || cmp < 0 {
		t.Skip("Requires server with FCV at least 4.4")
	}

	_, err = sessionProvider.GetNodeType()
	if err != nil {
		t.Fatalf("Could not get node type")
	}

	Convey("With a test MongoRestore instance", t, func() {
		testdb := session.Database(testDB)

		// Drop the collection to clean up resources
		//
		//nolint:errcheck
		defer testdb.Collection(testDB).Drop(ctx)

		args := []string{
			DirectoryOption, "testdata/commit_indexes_build",
			OplogReplayOption,
			DropOption,
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		// Run mongorestore
		result := restore.Restore()
		So(result.Err, ShouldBeNil)

		Convey(
			"RestoreOplog() should convert commitIndexBuild op to createIndexes cmd and build index",
			func() {
				destColl := session.Database("commit_index").Collection("test")
				indexes, _ := destColl.Indexes().List(context.Background())

				type indexSpec struct {
					Name, NS                string
					Key                     bson.D
					Unique                  bool    `bson:",omitempty"`
					DropDups                bool    `bson:"dropDups,omitempty"`
					Background              bool    `bson:",omitempty"`
					Sparse                  bool    `bson:",omitempty"`
					Bits                    int     `bson:",omitempty"`
					Min                     float64 `bson:",omitempty"`
					Max                     float64 `bson:",omitempty"`
					BucketSize              float64 `bson:"bucketSize,omitempty"`
					ExpireAfter             int     `bson:"expireAfterSeconds,omitempty"`
					Weights                 bson.D  `bson:",omitempty"`
					DefaultLanguage         string  `bson:"default_language,omitempty"`
					LanguageOverride        string  `bson:"language_override,omitempty"`
					TextIndexVersion        int     `bson:"textIndexVersion,omitempty"`
					PartialFilterExpression bson.M  `bson:"partialFilterExpression,omitempty"`

					Collation bson.D `bson:"collation,omitempty"`
				}

				indexCnt := 0
				for indexes.Next(context.Background()) {
					var index indexSpec
					err := indexes.Decode(&index)
					So(err, ShouldBeNil)
					indexCnt++
				}
				// Should create 3 indexes: _id and two others
				So(indexCnt, ShouldEqual, 3)
			},
		)
	})
}

// CreateIndexes oplog will be applied directly for versions < 4.4 and converted to createIndex cmd > 4.4.
func TestCreateIndexes(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	ctx := context.Background()
	testDB := "create_indexes"

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}
	defer sessionProvider.Close()

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("No client available")
	}

	_, err = sessionProvider.GetNodeType()
	if err != nil {
		t.Fatalf("Could not get node type")
	}

	Convey("With a test MongoRestore instance", t, func() {
		testdb := session.Database(testDB)

		// Drop the collection to clean up resources
		//
		//nolint:errcheck
		defer testdb.Collection(testDB).Drop(ctx)

		args := []string{
			DirectoryOption, "testdata/create_indexes",
			OplogReplayOption,
			DropOption,
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)

		defer restore.Close()

		// Run mongorestore
		result := restore.Restore()
		So(result.Err, ShouldBeNil)

		Convey(
			"RestoreOplog() should convert commitIndexBuild op to createIndexes cmd and build index",
			func() {
				destColl := session.Database("create_indexes").Collection("test")
				indexes, _ := destColl.Indexes().List(context.Background())

				type indexSpec struct {
					Name, NS                string
					Key                     bson.D
					Unique                  bool    `bson:",omitempty"`
					DropDups                bool    `bson:"dropDups,omitempty"`
					Background              bool    `bson:",omitempty"`
					Sparse                  bool    `bson:",omitempty"`
					Bits                    int     `bson:",omitempty"`
					Min                     float64 `bson:",omitempty"`
					Max                     float64 `bson:",omitempty"`
					BucketSize              float64 `bson:"bucketSize,omitempty"`
					ExpireAfter             int     `bson:"expireAfterSeconds,omitempty"`
					Weights                 bson.D  `bson:",omitempty"`
					DefaultLanguage         string  `bson:"default_language,omitempty"`
					LanguageOverride        string  `bson:"language_override,omitempty"`
					TextIndexVersion        int     `bson:"textIndexVersion,omitempty"`
					PartialFilterExpression bson.M  `bson:"partialFilterExpression,omitempty"`

					Collation bson.D `bson:"collation,omitempty"`
				}

				indexCnt := 0
				for indexes.Next(context.Background()) {
					var index indexSpec
					err := indexes.Decode(&index)
					So(err, ShouldBeNil)
					indexCnt++
				}
				// Should create 3 indexes: _id and two others
				So(indexCnt, ShouldEqual, 3)
			},
		)
	})
}

func TestGeoHaystackIndexes(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	ctx := context.Background()
	dbName := "geohaystack_test"

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}

	defer sessionProvider.Close()

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("No client available")
	}

	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "5.0"); err != nil || cmp < 0 {
		t.Skip("Requires server with FCV 5.0 or later")
	}

	Convey("With a test MongoRestore instance", t, func() {
		testdb := session.Database(dbName)

		// Drop the collection to clean up resources
		//
		//nolint:errcheck
		defer testdb.Collection("foo").Drop(ctx)

		args := []string{
			DirectoryOption, "testdata/coll_with_geohaystack_index",
			DropOption,
		}

		restore, err := getRestoreWithArgs(args...)
		So(err, ShouldBeNil)
		defer restore.Close()

		// Run mongorestore
		result := restore.Restore()
		So(result.Err, ShouldNotBeNil)

		So(result.Err.Error(), ShouldContainSubstring, "found a geoHaystack index")
	})
}

func createTimeseries(dbName, coll string, client *mongo.Client) {
	timeseriesOptions := bson.M{
		"timeField": "ts",
		"metaField": "meta",
	}
	createCmd := bson.D{
		{"create", coll},
		{"timeseries", timeseriesOptions},
	}
	client.Database(dbName).RunCommand(context.Background(), createCmd)
}

func TestUnversionedIndexes(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	ctx := context.Background()

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}

	defer sessionProvider.Close()

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("No client available")
	}

	dbName := t.Name()
	collName := "coll"

	coll := session.Database(dbName).Collection(collName)

	metadataEJSON, err := bson.MarshalExtJSON(
		bson.D{
			{"collectionName", collName},
			{"type", "collection"},
			{"uuid", uuid.New().String()},
			{"indexes", []bson.D{
				{
					{"v", 2},
					{"key", bson.D{{"_id", 1}}},
					{"name", "_id_"},
				},
				{
					{"v", 2},
					{"key", bson.D{{"myfield", "2dsphere"}}},
					{"name", "my2dsphere"},
				},
			}},
		},
		false,
		false,
	)
	require.NoError(t, err, "should marshal metadata to extJSON")

	archive := archive.SimpleArchive{
		CollectionMetadata: []archive.CollectionMetadata{
			{
				Database:   dbName,
				Collection: collName,
				Metadata:   string(metadataEJSON),
				Size:       0,
			},
		},
		Namespaces: []archive.SimpleNamespace{
			{
				Database:   dbName,
				Collection: collName,
			},
		},
	}
	archiveBytes, err := archive.Marshal()
	require.NoError(t, err, "should marshal the archive")

	withArchiveMongodump(t, func(archivePath string) {
		require.NoError(t, os.WriteFile(archivePath, archiveBytes, 0644))

		// Restore our altered archive:
		restore, err := getRestoreWithArgs(
			DropOption,
			ArchiveOption+"="+archivePath,
		)
		require.NoError(t, err)
		defer restore.Close()

		result := restore.Restore()
		require.NoError(t, result.Err, "can run mongorestore")
		require.EqualValues(t, 0, result.Failures, "mongorestore reports 0 failures")

		cursor, err := coll.Indexes().List(ctx)
		require.NoError(t, err, "should open index-list cursor")

		var indexes []idx.IndexDocument
		err = cursor.All(ctx, &indexes)
		require.NoError(t, err, "should fetch index specs")

		t.Logf("indexes: %+v", indexes)

		var twoDIndexDoc idx.IndexDocument

		for _, idx := range indexes {
			if idx.Options["name"] == "my2dsphere" {
				twoDIndexDoc = idx
			}
		}

		require.NotNil(t, twoDIndexDoc.Key, "should find 2dsphere index (indexes: %+v)", indexes)
		assert.EqualValues(t, 1, twoDIndexDoc.Options["2dsphereIndexVersion"])
	})
}

func TestRestoreTimeseriesCollectionsWithMixedSchema(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	ctx := context.Background()

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}

	defer sessionProvider.Close()

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("No client available")
	}

	fcv := testutil.GetFCV(session)
	// TODO: Enable tests for 6.0, 7.0 and 8.0 (TOOLS-3597).
	// The server fix for SERVER-84531 was only backported to 7.3.
	if cmp, err := testutil.CompareFCV(fcv, "7.3"); cmp < 0 {
		if err != nil {
			t.Fatalf("Failed to get FCV: %v", err)
		}
		t.Skip("Requires server with FCV 7.3 or later")
	}

	if cmp, err := testutil.CompareFCV(fcv, "8.0"); cmp >= 0 {
		if err != nil {
			t.Fatalf("Failed to get FCV: %v", err)
		}
		t.Skip("The test currently fails on v8.0 because of SERVER-92222")
	}

	dbName := "timeseries_test_DB"
	collName := "timeseries_mixed_schema"
	testdb := session.Database(dbName)
	bucketColl := testdb.Collection("system.buckets." + collName)

	require.NoError(t, setupTimeseriesWithMixedSchema(dbName, collName))

	withArchiveMongodump(t, func(file string) {
		require.NoError(t, testdb.Collection(collName).Drop(ctx))
		require.NoError(t, bucketColl.Drop(ctx))

		restore, err := getRestoreWithArgs(
			DropOption,
			ArchiveOption+"="+file,
		)
		require.NoError(t, err)
		defer restore.Close()

		result := restore.Restore()
		require.NoError(t, result.Err, "can run mongorestore")
		require.EqualValues(t, 0, result.Failures, "mongorestore reports 0 failures")

		count, err := testdb.Collection(collName).CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		require.Equal(t, int64(2), count)

		count, err = bucketColl.CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		require.Equal(t, int64(1), count)

		hasMixedSchema, err := timeseriesBucketsMayHaveMixedSchemaData(bucketColl)
		require.NoError(t, err)
		require.True(t, hasMixedSchema)

		//nolint:errcheck
		defer testdb.Collection(collName).Drop(ctx)
	})
}

func timeseriesBucketsMayHaveMixedSchemaData(bucketColl *mongo.Collection) (bool, error) {
	ctx := context.Background()
	cursor, err := bucketColl.Database().RunCommandCursor(ctx, bson.D{
		{"aggregate", bucketColl.Name()},
		{"pipeline", bson.A{
			bson.D{{"$listCatalog", bson.D{}}},
		}},
		{"readConcern", bson.D{{"level", "majority"}}},
		{"cursor", bson.D{}},
	})
	if err != nil {
		return false, err
	}

	if !cursor.Next(ctx) {
		return false, fmt.Errorf("no entry in $listCatalog response")
	}

	md, err := cursor.Current.LookupErr("md")
	if err != nil {
		return false, err
	}

	hasMixedSchema, err := md.Document().LookupErr("timeseriesBucketsMayHaveMixedSchemaData")
	if err != nil {
		return false, err
	}

	return hasMixedSchema.Boolean(), nil
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
	if cmp, err := testutil.CompareFCV(testutil.GetFCV(client), "7.3"); err != nil || cmp >= 0 {
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

func TestRestoreTimeseriesCollections(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)
	ctx := context.Background()
	dbName := "timeseries_test"

	sessionProvider, _, err := testutil.GetBareSessionProvider()
	if err != nil {
		t.Fatalf("No cluster available: %v", err)
	}

	defer sessionProvider.Close()

	session, err := sessionProvider.GetSession()
	if err != nil {
		t.Fatalf("No client available")
	}

	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "5.0"); err != nil || cmp < 0 {
		t.Skip("Requires server with FCV 5.0 or later")
	}

	Convey("With a test MongoRestore instance", t, func() {
		testdb := session.Database(dbName)

		// Drop the collection to clean up resources
		//
		//nolint:errcheck
		defer testdb.Drop(ctx)

		args := []string{}
		var restore *MongoRestore

		Convey("restoring a directory should succeed", func() {
			args = append(args, DirectoryOption, "testdata/timeseries_tests/ts_dump")
			restore, err = getRestoreWithArgs(args...)

			So(err, ShouldBeNil)

		})

		Convey("restoring an archive should succeed", func() {
			args = append(args, ArchiveOption+"=testdata/timeseries_tests/dump.archive")
			restore, err = getRestoreWithArgs(args...)

			So(err, ShouldBeNil)
		})

		Convey("restoring an archive from stdin should succeed", func() {
			args = append(args, ArchiveOption+"=-")
			restore, err = getRestoreWithArgs(args...)

			archiveFile, err := os.Open("testdata/timeseries_tests/dump.archive")
			So(err, ShouldBeNil)
			restore.InputReader = archiveFile
		})
		defer restore.Close()

		// Run mongorestore
		result := restore.Restore()
		So(result.Err, ShouldBeNil)
		So(result.Successes, ShouldEqual, 10)
		So(result.Failures, ShouldEqual, 0)

		count, err := testdb.Collection("foo_ts").CountDocuments(context.Background(), bson.M{})
		So(err, ShouldBeNil)
		So(count, ShouldEqual, 1000)

		count, err = testdb.Collection("system.buckets.foo_ts").
			CountDocuments(context.Background(), bson.M{})
		So(err, ShouldBeNil)
		So(count, ShouldEqual, 10)
	})

	Convey("With a test MongoRestore instance", t, func() {
		testdb := session.Database(dbName)

		// Drop the collection to clean up resources
		//
		//nolint:errcheck
		defer testdb.Drop(ctx)

		args := []string{}

		Convey(
			"restoring a timeseries collection that already exists on the destination should fail",
			func() {
				createTimeseries(dbName, "foo_ts", session)
				args = append(args, DirectoryOption, "testdata/timeseries_tests/ts_dump")
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldNotBeNil)
			},
		)

		Convey(
			"restoring a timeseries collection when the system.buckets collection already exists on the destination should fail",
			func() {
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)
				// In the 8.0 release, this no longer leads to an error, so
				// there's nothing to test here.
				if restore.serverVersion.GTE(db.Version{8, 0, 0}) {
					SkipSo()
					return
				}

				testdb.RunCommand(context.Background(), bson.M{"create": "system.buckets.foo_ts"})
				args = append(args, DirectoryOption, "testdata/timeseries_tests/ts_dump")

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldNotBeNil)
			},
		)

		Convey(
			"restoring a timeseries collection with --oplogReplay should apply changes to the system.buckets collection correctly",
			func() {
				args = append(
					args,
					DirectoryOption,
					"testdata/timeseries_tests/ts_dump_with_oplog",
					OplogReplayOption,
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 10)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 2164)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 10)
			},
		)

		Convey(
			"restoring a timeseries collection that already exists on the destination with --drop should succeed",
			func() {
				createTimeseries(dbName, "foo_ts", session)
				args = append(
					args,
					DirectoryOption,
					"testdata/timeseries_tests/ts_dump",
					DropOption,
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 10)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1000)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 10)
			},
		)

		Convey("restoring a timeseries collection with --noOptionsRestore should fail", func() {
			args = append(
				args,
				DirectoryOption,
				"testdata/timeseries_tests/ts_dump",
				NoOptionsRestoreOption,
			)
			restore, err := getRestoreWithArgs(args...)
			So(err, ShouldBeNil)

			result := restore.Restore()
			defer restore.Close()
			So(result.Err, ShouldNotBeNil)
		})

		Convey(
			"restoring a timeseries collection with invalid system.buckets should fail validation",
			func() {
				args = append(
					args,
					DirectoryOption,
					"testdata/timeseries_tests/ts_dump_invalid_buckets",
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 0)
				So(result.Failures, ShouldEqual, 5)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)
			},
		)

		Convey(
			"restoring a timeseries collection with invalid system.buckets should fail validation even with --bypassDocumentValidation",
			func() {
				args = append(
					args,
					DirectoryOption,
					"testdata/timeseries_tests/ts_dump_invalid_buckets",
					BypassDocumentValidationOption,
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 0)
				So(result.Failures, ShouldEqual, 5)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)
			},
		)

		Convey(
			"timeseries collection should be restored if the system.buckets BSON file is used and the metadata exists",
			func() {
				args = append(
					args,
					DBOption,
					dbName,
					CollectionOption,
					"foo_ts",
					"testdata/timeseries_tests/ts_dump/timeseries_test/system.buckets.foo_ts.bson",
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 10)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1000)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 10)
			},
		)

		Convey(
			"timeseries collection should be restored if the system.buckets BSON file is used and the metadata exists and it should be renamed to --collection",
			func() {
				args = append(
					args,
					DBOption,
					dbName,
					CollectionOption,
					"bar_ts",
					"testdata/timeseries_tests/ts_dump/timeseries_test/system.buckets.foo_ts.bson",
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 10)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("bar_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1000)

				count, err = testdb.Collection("system.buckets.bar_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 10)
			},
		)

		Convey(
			"timeseries collection should be restored with system.buckets BSON file as target and the metadata exists without db option and collection option",
			func() {
				args = append(
					args,
					"testdata/timeseries_tests/ts_dump/timeseries_test/system.buckets.foo_ts.bson",
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 10)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1000)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 10)
			},
		)

		Convey(
			"restoring a single system.buckets BSON file (with no metadata) should fail",
			func() {
				args = append(
					args,
					DBOption,
					dbName,
					CollectionOption,
					"system.buckets.foo_ts",
					"testdata/timeseries_tests/ts_single_buckets_file/system.buckets.foo_ts.bson",
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldNotBeNil)
			},
		)

		Convey(
			"system.buckets should be restored if the timeseries collection is included in --nsInclude",
			func() {
				args = append(
					args,
					NSIncludeOption,
					dbName+".foo_ts",
					DirectoryOption,
					"testdata/timeseries_tests/ts_dump",
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 10)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1000)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 10)
			},
		)

		Convey(
			"system.buckets should not be restored if the timeseries collection is not included in --nsInclude",
			func() {
				args = append(
					args,
					NSIncludeOption,
					dbName+".system.buckets.foo_ts",
					DirectoryOption,
					"testdata/timeseries_tests/ts_dump",
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 0)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)
			},
		)

		Convey(
			"system.buckets should not be restored if the timeseries collection is excluded by --nsExclude",
			func() {
				args = append(
					args,
					NSExcludeOption,
					dbName+".foo_ts",
					DirectoryOption,
					"testdata/timeseries_tests/ts_dump",
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 0)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)
			},
		)

		Convey(
			"--noIndexRestore should stop secondary indexes from being built but should have no impact on the clustered index of system.buckets",
			func() {
				args = append(
					args,
					DirectoryOption,
					"testdata/timeseries_tests/ts_dump",
					NoIndexRestoreOption,
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 10)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1000)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 10)

				indexes, err := testdb.Collection("foo_ts").Indexes().List(ctx)
				//nolint:staticcheck
				defer indexes.Close(ctx)
				So(err, ShouldBeNil)

				numIndexes := 0
				for indexes.Next(ctx) {
					numIndexes++
				}

				if (restore.serverVersion.GTE(db.Version{6, 3, 0})) {
					Convey(
						"--noIndexRestore should build the index on meta, time by default for time-series collections if server version >= 6.3.0",
						func() {
							So(numIndexes, ShouldEqual, 1)
						},
					)
				} else {
					So(numIndexes, ShouldEqual, 0)
				}

				cur, err := testdb.ListCollections(ctx, bson.M{"name": "system.buckets.foo_ts"})
				So(err, ShouldBeNil)

				for cur.Next(ctx) {
					optVal, err := cur.Current.LookupErr("options")
					So(err, ShouldBeNil)

					optRaw, ok := optVal.DocumentOK()
					So(ok, ShouldBeTrue)

					clusteredIdxVal, err := optRaw.LookupErr("clusteredIndex")
					So(err, ShouldBeNil)

					clusteredIdx := clusteredIdxVal.Boolean()
					So(clusteredIdx, ShouldBeTrue)
				}
			},
		)

		Convey("system.buckets should be renamed if the timeseries collection is renamed", func() {
			args = append(
				args,
				NSFromOption,
				dbName+".foo_ts",
				NSToOption,
				dbName+".foo_rename_ts",
				DirectoryOption,
				"testdata/timeseries_tests/ts_dump",
			)
			restore, err := getRestoreWithArgs(args...)
			So(err, ShouldBeNil)

			result := restore.Restore()
			defer restore.Close()
			So(result.Err, ShouldBeNil)
			So(result.Successes, ShouldEqual, 10)
			So(result.Failures, ShouldEqual, 0)

			count, err := testdb.Collection("foo_ts").CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 0)

			count, err = testdb.Collection("system.buckets.foo_ts").
				CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 0)

			count, err = testdb.Collection("foo_rename_ts").
				CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 1000)

			count, err = testdb.Collection("system.buckets.foo_rename_ts").
				CountDocuments(context.Background(), bson.M{})
			So(err, ShouldBeNil)
			So(count, ShouldEqual, 10)
		})

		Convey(
			"system.buckets collection should not be renamed if the timeseries collection is not renamed, even if the user tries to rename the system.buckets collection",
			func() {
				args = append(
					args,
					NSFromOption,
					dbName+".system.buckets.foo_ts",
					NSToOption,
					dbName+".system.buckets.foo_rename_ts",
					DirectoryOption,
					"testdata/timeseries_tests/ts_dump",
				)
				restore, err := getRestoreWithArgs(args...)
				So(err, ShouldBeNil)

				result := restore.Restore()
				defer restore.Close()
				So(result.Err, ShouldBeNil)
				So(result.Successes, ShouldEqual, 10)
				So(result.Failures, ShouldEqual, 0)

				count, err := testdb.Collection("foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1000)

				count, err = testdb.Collection("system.buckets.foo_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 10)

				count, err = testdb.Collection("foo_rename_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)

				count, err = testdb.Collection("system.buckets.foo_rename_ts").
					CountDocuments(context.Background(), bson.M{})
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 0)
			},
		)
	})
}

// ----------------------------------------------------------------------
// All tests from this point onwards use testify, not convey. See the
// CONTRIBUING.md file in the top level of the repo for details on how to
// write tests using testify.
// ----------------------------------------------------------------------

type indexInfo struct {
	name string
	keys []string
}

func TestRestoreClusteredIndex(t *testing.T) {
	require := require.New(t)

	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	session, err := testutil.GetBareSession()
	require.NoError(err, "can connect to server")

	fcv := testutil.GetFCV(session)
	if cmp, err := testutil.CompareFCV(fcv, "5.3"); err != nil || cmp < 0 {
		t.Skipf("Requires server with FCV 5.3 or later and we have %s", fcv)
	}

	t.Run("restore from dump with default index name", func(t *testing.T) {
		testRestoreClusteredIndexFromDump(t, "")
	})
	t.Run("restore from dump with custom index name", func(t *testing.T) {
		testRestoreClusteredIndexFromDump(t, "custom index name")
	})

	res := session.Database("admin").RunCommand(context.Background(), bson.M{"replSetGetStatus": 1})
	if res.Err() != nil {
		t.Skip("server is not part of a replicaset so we cannot test restore from oplog")
	}
	t.Run("restore from oplog with default index name", func(t *testing.T) {
		testRestoreClusteredIndexFromOplog(t, "")
	})
	t.Run("restore from oplog with default index name", func(t *testing.T) {
		testRestoreClusteredIndexFromOplog(t, "custom index name")
	})
}

func TestRestoreZeroTimestamp(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	ctx := context.Background()

	require := require.New(t)

	session, err := testutil.GetBareSession()
	require.NoError(err, "can connect to server")

	dbName := uniqueDBName()
	testDB := session.Database(dbName)
	defer func() {
		err = testDB.Drop(ctx)
		if err != nil {
			t.Fatalf("Failed to drop test database: %v", err)
		}
	}()

	coll := testDB.Collection("mycoll")

	docID := primitive.Timestamp{}

	_, err = coll.UpdateOne(
		ctx,
		bson.D{
			{"_id", docID},
		},
		mongo.Pipeline{
			{{"$replaceRoot", bson.D{
				{"newRoot", bson.D{
					{"$literal", bson.D{
						{"empty_time", primitive.Timestamp{}},
						{"other", "$$ROOT"},
					}},
				}},
			}}},
		},
		mopt.Update().SetUpsert(true),
	)
	require.NoError(err, "should insert (via update/upsert)")

	withBSONMongodumpForCollection(t, coll.Database().Name(), coll.Name(), func(dir string) {
		restore, err := getRestoreWithArgs(
			DropOption,
			dir,
		)
		require.NoError(err)
		defer restore.Close()

		result := restore.Restore()
		require.NoError(result.Err, "can run mongorestore (result: %+v)", result)
		require.EqualValues(0, result.Failures, "mongorestore reports 0 failures")
	})

	cursor, err := coll.Find(ctx, bson.D{})
	require.NoError(err, "should find docs")
	docs := []bson.M{}
	require.NoError(cursor.All(ctx, &docs), "should read docs")

	require.Len(docs, 1, "expect docs count")
	assert.Equal(
		t,
		bson.M{
			"_id":        docID,
			"empty_time": primitive.Timestamp{},
			"other":      "$$ROOT",
		},
		docs[0],
		"expect empty timestamp restored",
	)
}

func TestRestoreZeroTimestamp_NonClobber(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	ctx := context.Background()

	require := require.New(t)

	session, err := testutil.GetBareSession()
	require.NoError(err, "can connect to server")

	dbName := uniqueDBName()
	testDB := session.Database(dbName)
	defer func() {
		err = testDB.Drop(ctx)
		if err != nil {
			t.Fatalf("Failed to drop test database: %v", err)
		}
	}()

	coll := testDB.Collection("mycoll")

	docID := strings.Repeat("x", 7)

	_, err = coll.UpdateOne(
		ctx,
		bson.D{
			{"_id", docID},
		},
		mongo.Pipeline{
			{{"$replaceRoot", bson.D{
				{"newRoot", bson.D{
					{"empty_time", primitive.Timestamp{}},
				}},
			}}},
		},
		mopt.Update().SetUpsert(true),
	)
	require.NoError(err, "should insert (via update/upsert)")

	withBSONMongodumpForCollection(t, coll.Database().Name(), coll.Name(), func(dir string) {
		updated, err := coll.UpdateOne(
			ctx,
			bson.D{
				{"_id", docID},
			},
			mongo.Pipeline{
				{{"$replaceRoot", bson.D{
					{"newRoot", bson.D{
						{"nonempty_time", primitive.Timestamp{1, 2}},
					}},
				}}},
			},
		)
		require.NoError(err, "should send update")
		require.NotZero(updated.MatchedCount, "update should match a doc")

		restore, err := getRestoreWithArgs(
			dir,
		)
		require.NoError(err)
		defer restore.Close()

		result := restore.Restore()
		require.NoError(result.Err, "can run mongorestore")

		assert.EqualValues(t, 1, result.Failures, "mongorestore reports failure")
	})

	cursor, err := coll.Find(ctx, bson.D{})
	require.NoError(err, "should find docs")
	docs := []bson.M{}
	require.NoError(cursor.All(ctx, &docs), "should read docs")

	require.Len(docs, 1, "expect docs count")
	assert.NotContains(
		t,
		docs[0],
		"empty_time",
		"restore did not clobber existing document (found: %+v)",
		docs[0],
	)
}

func testRestoreClusteredIndexFromDump(t *testing.T, indexName string) {
	require := require.New(t)

	session, err := testutil.GetBareSession()
	require.NoError(err, "can connect to server")

	dbName := uniqueDBName()
	testDB := session.Database(dbName)
	defer func() {
		err = testDB.Drop(context.Background())
		if err != nil {
			t.Fatalf("Failed to drop test database: %v", err)
		}
	}()

	dataLen := createClusteredIndex(t, testDB, indexName)

	withBSONMongodumpForCollection(t, testDB.Name(), "stocks", func(dir string) {
		restore, err := getRestoreWithArgs(
			DropOption,
			dir,
		)
		require.NoError(err)
		defer restore.Close()

		result := restore.Restore()
		require.NoError(result.Err, "can run mongorestore")
		require.EqualValues(dataLen, result.Successes, "mongorestore reports %d successes", dataLen)
		require.EqualValues(0, result.Failures, "mongorestore reports 0 failures")

		assertClusteredIndex(t, testDB, indexName)
	})
}

func testRestoreClusteredIndexFromOplog(t *testing.T, indexName string) {
	require := require.New(t)

	session, err := testutil.GetBareSession()
	require.NoError(err, "can connect to server")

	dbName := uniqueDBName()
	testDB := session.Database(dbName)
	defer func() {
		err = testDB.Drop(context.Background())
		if err != nil {
			t.Fatalf("Failed to drop test database: %v", err)
		}
	}()

	createClusteredIndex(t, testDB, indexName)

	withOplogMongoDump(t, dbName, "stocks", func(dir string) {
		restore, err := getRestoreWithArgs(
			DropOption,
			OplogReplayOption,
			dir,
		)
		require.NoError(err)
		defer restore.Close()

		result := restore.Restore()
		require.NoError(result.Err, "can run mongorestore")
		require.EqualValues(0, result.Successes, "mongorestore reports 0 successes")
		require.EqualValues(0, result.Failures, "mongorestore reports 0 failures")

		assertClusteredIndex(t, testDB, indexName)
	})
}

func createClusteredIndex(t *testing.T, testDB *mongo.Database, indexName string) int {
	require := require.New(t)

	indexOpts := bson.M{
		"key":    bson.M{"_id": 1},
		"unique": true,
	}
	if indexName != "" {
		indexOpts["name"] = indexName
	}
	createCollCmd := bson.D{
		{Key: "create", Value: "stocks"},
		{Key: "clusteredIndex", Value: indexOpts},
	}
	res := testDB.RunCommand(context.Background(), createCollCmd, nil)
	require.NoError(res.Err(), "can create a clustered collection")

	var r interface{}
	err := res.Decode(&r)
	require.NoError(err)

	stocks := testDB.Collection("stocks")
	stockData := []interface{}{
		bson.M{"ticker": "MDB", "price": 245.33},
		bson.M{"ticker": "GOOG", "price": 2214.91},
		bson.M{"ticker": "BLZE", "price": 6.23},
	}
	_, err = stocks.InsertMany(context.Background(), stockData)
	require.NoError(err, "can insert documents into collection")

	return len(stockData)
}

func assertClusteredIndex(t *testing.T, testDB *mongo.Database, indexName string) {
	require := require.New(t)

	c, err := testDB.ListCollections(context.Background(), bson.M{})
	require.NoError(err, "can get list of collections")

	type collectionRes struct {
		Name    string
		Type    string
		Options bson.M
		Info    bson.D
		IdIndex bson.D
	}

	var collections []collectionRes
	// two Indexes should be created in addition to the _id, foo and foo_2
	for c.Next(context.Background()) {
		var res collectionRes
		err = c.Decode(&res)
		require.NoError(err, "can decode collection result")
		collections = append(collections, res)
	}

	require.Len(collections, 1, "database has one collection")
	require.Equal("stocks", collections[0].Name, "collection is named stocks")
	idx := clusteredIndexInfo(t, collections[0].Options)
	expectName := indexName
	if expectName == "" {
		expectName = "_id_"
	}
	require.Equal(expectName, idx.name, "index is named '%s'", expectName)
	require.Equal([]string{"_id"}, idx.keys, "index key is the '_id' field")
}

func clusteredIndexInfo(t *testing.T, options bson.M) indexInfo {
	idx, found := options["clusteredIndex"]
	require.True(t, found, "options has key named 'clusteredIndex'")
	require.IsType(t, bson.M{}, idx, "idx value is a bson.M")

	//nolint:errcheck
	idxM := idx.(bson.M)
	name, found := idxM["name"]
	require.True(t, found, "index has a key named 'name'")
	require.IsType(t, "string", name, "key value is a string")

	keys, found := idxM["key"]
	require.True(t, found, "index has a key named 'key'")
	require.IsType(t, bson.M{}, keys, "key value is a bson.M")

	keysM, ok := keys.(bson.M)
	require.True(t, ok)

	var keyNames []string
	for k := range keysM {
		keyNames = append(keyNames, k)
	}

	nameStr, ok := name.(string)
	require.True(t, ok)

	return indexInfo{
		name: nameStr,
		keys: keyNames,
	}
}

func withBSONMongodump(t *testing.T, testCase func(string), args ...string) {
	dir, cleanup := testutil.MakeTempDir(t)
	defer cleanup()
	dirArgs := []string{
		"--out", dir,
	}
	runMongodumpWithArgs(t, append(dirArgs, args...)...)
	testCase(dir)
}

func withBSONMongodumpForCollection(
	t *testing.T,
	db string,
	collection string,
	testCase func(string),
) {
	dir, cleanup := testutil.MakeTempDir(t)
	defer cleanup()
	runBSONMongodumpForCollection(t, dir, db, collection)
	testCase(dir)
}

func withOplogMongoDump(t *testing.T, db string, collection string, testCase func(string)) {
	require := require.New(t)

	dir, cleanup := testutil.MakeTempDir(t)
	defer cleanup()

	// This queries the local.oplog.rs collection for commands or CRUD
	// operations on the collection we are testing (which will have a unique
	// name for each test).
	query := map[string]interface{}{
		"$or": []map[string]string{
			{"ns": fmt.Sprintf("%s.$cmd", db)},
			{"ns": fmt.Sprintf("%s.%s", db, collection)},
		},
	}
	q, err := json.Marshal(query)
	require.NoError(err, "can marshal query to JSON")

	// We dump just the documents matching the query using mongodump "normally".
	bsonFile := runBSONMongodumpForCollection(t, dir, "local", "oplog.rs", "--query", string(q))

	// Then we take the BSON dump file and rename it to "oplog.bson" and put
	// it in the root of the dump directory.
	newPath := filepath.Join(dir, "oplog.bson")
	err = os.Rename(bsonFile, newPath)
	require.NoError(err, "can rename %s -> %s", bsonFile, newPath)

	// Finally, we remove the "local" dir created by mongodump so that
	// mongorestore doesn't see it.
	localDir := filepath.Join(dir, "local")
	err = os.RemoveAll(localDir)
	require.NoError(err, "can remove %s", localDir)

	// With all that done, we now have a tree on disk like this:
	//
	// /tmp/mongorestore_test1152384390
	// └── oplog.bson
	//
	// We can run `mongorestore --oplogReplay /tmp/mongorestore_test1152384390`
	// to do a restore from the oplog.bson file.

	testCase(dir)
}

func runBSONMongodumpForCollection(
	t *testing.T,
	dir, db, collection string,
	args ...string,
) string {
	require := require.New(t)
	baseArgs := []string{
		"--out", dir,
		"--db", db,
		"--collection", collection,
	}
	runMongodumpWithArgs(
		t,
		append(baseArgs, args...)...,
	)
	bsonFile := filepath.Join(dir, db, fmt.Sprintf("%s.bson", collection))
	_, err := os.Stat(bsonFile)
	require.NoError(err, "dump created BSON data file")
	_, err = os.Stat(filepath.Join(dir, db, fmt.Sprintf("%s.metadata.json", collection)))
	require.NoError(err, "dump created JSON metadata file")
	return bsonFile
}

func runMongodumpWithArgs(t *testing.T, args ...string) {
	require := require.New(t)
	cmd := []string{"go", "run", filepath.Join("..", "mongodump", "main")}
	cmd = append(cmd, testutil.GetBareArgs()...)
	cmd = append(cmd, args...)
	out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	cmdStr := strings.Join(cmd, " ")
	require.NoError(err, "can execute command %s with output: %s", cmdStr, out)
	require.NotContains(
		string(out),
		"does not exist",
		"running [%s] does not tell us the namespace does not exist",
		cmdStr,
	)
}

func uniqueDBName() string {
	return fmt.Sprintf("mongorestore_test_%d_%d", os.Getpid(), time.Now().UnixMilli())
}

func TestPipedDumpRestore(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	t.Logf("start %#q", t.Name())
	// TODO: Make this t.Context() once we move to Go 1.24.
	ctx := context.Background()

	provider, _, err := testutil.GetBareSessionProvider()
	require.NoError(t, err, "should get session provider")

	t.Logf("getting session")
	sess, err := provider.GetSession()
	require.NoError(t, err, "should get session")

	srcCollNames := []string{"alpha", "beta", "gamma", "delta", "epsilon"}

	db := sess.Database(uniqueDBName())
	require.NoError(t, db.Drop(ctx), "should pre-drop DB %#q", db.Name())

	t.Logf("creating collections")

	for _, collName := range srcCollNames {
		docs := lo.RepeatBy(
			10_000,
			func(_ int) bson.D {
				return bson.D{
					{"someNum", rand.Float64()},
				}
			},
		)

		require.NoError(
			t,
			db.Collection(collName).Drop(ctx),
			"should drop %#q", collName,
		)

		_, err := db.Collection(collName).InsertMany(
			ctx,
			lo.ToAnySlice(docs),
		)

		require.NoError(t, err, "should insert docs into %#q", collName)
	}

	t.Log("Finished creating documents.")

	reader, writer := io.Pipe()

	eg, _ := errgroup.WithContext(ctx)
	eg.Go(func() error {
		defer writer.Close()

		dump, err := GetArchiveMongoDump(writer)
		if err != nil {
			return errors.Wrap(err, "create mongodump")
		}

		dump.ToolOptions.DB = db.Name()

		assert.NoError(t, dump.Dump(), "dump should work")

		return nil
	})

	eg.Go(func() error {
		defer reader.Close()

		restore, err := GetArchiveMongoRestore(reader)
		if err != nil {
			return errors.Wrap(err, "create mongorestore")
		}

		restore.NSOptions = &NSOptions{
			NSFrom: lo.Map(
				srcCollNames,
				func(cn string, _ int) string {
					return db.Name() + "." + cn
				},
			),
			NSTo: lo.Map(
				srcCollNames,
				func(cn string, _ int) string {
					return db.Name() + ".dst-" + cn
				},
			),
		}

		assert.NoError(t, restore.Restore().Err, "restore should work")

		return nil
	})

	require.NoError(t, eg.Wait())
}

func TestRestoreMultipleIDIndexes(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	cases := []struct {
		Label   string
		Indexes []mongo.IndexModel
	}{
		{
			Label: "single simple hashed ID index",
			Indexes: []mongo.IndexModel{
				{Keys: bson.D{{"_id", "hashed"}}},
			},
		},
		{
			Label: "multihashed collations 2dsphere",
			Indexes: []mongo.IndexModel{
				{Keys: bson.D{{"_id", "hashed"}}},
				{
					Keys: bson.D{{"_id", "hashed"}},
					Options: mopt.Index().
						SetName("_id_hashed_de").
						SetCollation(&mopt.Collation{Locale: "de"}),
				},
				{
					Keys: bson.D{{"_id", "hashed"}},
					Options: mopt.Index().
						SetName("_id_hashed_ar").
						SetCollation(&mopt.Collation{Locale: "ar"}),
				},
				{Keys: bson.D{{"_id", "2dsphere"}}},
			},
		},
	}

	dbName := uniqueDBName()

	for c := range cases {
		curCase := cases[c]
		indexesToCreate := curCase.Indexes

		t.Run(
			curCase.Label,
			func(t *testing.T) {
				for attemptNum := range [20]any{} {
					attemptNum := attemptNum

					t.Run(
						fmt.Sprintf("attempt %d", attemptNum),
						func(t *testing.T) {
							session, err := testutil.GetBareSession()
							require.NoError(t, err, "should connect to server")

							ctx := context.Background()

							testDB := session.Database(dbName)

							collName := strings.ReplaceAll(
								fmt.Sprintf("%s %d", curCase.Label, attemptNum),
								" ",
								"-",
							)
							coll := testDB.Collection(collName)

							_, err = coll.Indexes().CreateMany(ctx, indexesToCreate)
							require.NoError(t, err, "indexes should be created")

							archivedIndexes := []bson.M{}
							require.NoError(
								t,
								listIndexes(ctx, coll, &archivedIndexes),
								"should list indexes",
							)

							withBSONMongodumpForCollection(
								t,
								testDB.Name(),
								coll.Name(),
								func(dir string) {
									restore, err := getRestoreWithArgs(
										DropOption,
										dir,
									)
									require.NoError(t, err)
									defer restore.Close()

									result := restore.Restore()
									require.NoError(
										t,
										result.Err,
										"%s: mongorestore should finish OK",
										curCase.Label,
									)
									require.EqualValues(
										t,
										0,
										result.Failures,
										"%s: mongorestore should report 0 failures",
										curCase.Label,
									)
								},
							)

							restoredIndexes := []bson.M{}
							require.NoError(
								t,
								listIndexes(ctx, coll, &restoredIndexes),
								"should list indexes",
							)

							assert.ElementsMatch(
								t,
								archivedIndexes,
								restoredIndexes,
								"indexes should round-trip dump/restore (attempt #%d)",
								1+attemptNum,
							)
						},
					)
				}
			},
		)

	}
}

// ListSpecifications returns IndexSpecifications, which don’t describe all
// parts of the index. So we need to List() the indexes directly and marshal
// them to something that lets us compare everything.
func listIndexes[T any](ctx context.Context, coll *mongo.Collection, target *T) error {
	ns := coll.Database().Name() + "." + coll.Name()

	cursor, err := coll.Indexes().List(ctx)
	if err != nil {
		return fmt.Errorf("failed to start listing indexes for %#q: %w", ns, err)
	}
	err = cursor.All(ctx, target)
	if err != nil {
		return fmt.Errorf("failed to list indexes for %#q: %w", ns, err)
	}

	return nil
}

func TestDumpAndRestoreConfigDB(t *testing.T) {
	require := require.New(t)

	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	_, err := testutil.GetBareSession()
	require.NoError(err, "can connect to server")

	t.Run(
		"test dump and restore only config db includes all config collections",
		func(t *testing.T) {
			testDumpAndRestoreConfigDBIncludesAllCollections(t)
		},
	)

	t.Run(
		"test dump and restore all dbs includes only some config collections",
		func(t *testing.T) {
			testDumpAndRestoreAllDBsIgnoresSomeConfigCollections(t)
		},
	)
}

func testDumpAndRestoreConfigDBIncludesAllCollections(t *testing.T) {
	require := require.New(t)

	session, err := testutil.GetBareSession()
	require.NoError(err, "can connect to server")

	configDB := session.Database("config")

	collections := createCollectionsWithTestDocuments(
		t,
		configDB,
		append(configCollectionNamesToKeep, userDefinedConfigCollectionNames...),
	)
	defer clearDB(t, configDB)

	withBSONMongodump(
		t,
		func(dir string) {

			clearDB(t, configDB)

			restore, err := getRestoreWithArgs(dir)
			require.NoError(err)
			defer restore.Close()

			result := restore.Restore()
			require.NoError(result.Err, "can run mongorestore")
			require.EqualValues(0, result.Failures, "mongorestore reports 0 failures")

			for _, collection := range collections {
				r := collection.FindOne(context.Background(), testDocument)
				require.NoError(r.Err(), "expected document")
			}

		},
		"--db", "config",
		"--excludeCollection", "transactions",
	)
}

func testDumpAndRestoreAllDBsIgnoresSomeConfigCollections(t *testing.T) {
	require := require.New(t)

	session, err := testutil.GetBareSession()
	require.NoError(err, "can connect to server")

	configDB := session.Database("config")

	userDefinedCollections := createCollectionsWithTestDocuments(
		t,
		configDB,
		userDefinedConfigCollectionNames,
	)
	collectionsToKeep := createCollectionsWithTestDocuments(
		t,
		configDB,
		configCollectionNamesToKeep,
	)
	defer clearDB(t, configDB)

	withBSONMongodump(
		t,
		func(dir string) {

			clearDB(t, configDB)

			restore, err := getRestoreWithArgs(
				DropOption,
				dir,
			)
			require.NoError(err)
			defer restore.Close()

			result := restore.Restore()
			require.NoError(result.Err, "can run mongorestore")
			require.EqualValues(0, result.Failures, "mongorestore reports 0 failures")

			for _, collection := range collectionsToKeep {
				r := collection.FindOne(context.Background(), testDocument)
				require.NoError(r.Err(), "expected document")
			}

			for _, collection := range userDefinedCollections {
				r := collection.FindOne(context.Background(), testDocument)
				require.Error(r.Err(), "expected no document")
			}

		},
	)
}

func TestFinalNewlinesInNamespaces(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.IntegrationTestType)

	ctx := context.Background()
	require := require.New(t)

	session, err := testutil.GetBareSession()
	require.NoError(err, "can connect to server")

	allNames := []string{
		"no-nl",
		"\ninitial-nl",
		"mid-\n-nl",
		"final-nl\n",
		"\ninitial-and-final-nl\n",
		"\nnl-\n-everywhere\n",
	}

	nlVariants := []struct {
		label string
		nl    string
	}{
		{"LF", "\n"},
		{"CR", "\r"},
		{"CRLF", "\r\n"},
	}

	for _, variant := range nlVariants {
		myAllNames := lo.Map(
			allNames,
			func(name string, _ int) string {
				return strings.ReplaceAll(name, "\n", variant.nl)
			},
		)

		t.Run(
			variant.label,
			func(t *testing.T) {
				for _, dbname := range myAllNames {
					dbname := dbname

					t.Run(
						fmt.Sprintf("dbname=%s", strconv.Quote(dbname)),
						func(t *testing.T) {
							require.NoError(session.Database(dbname).Drop(ctx))
							createCollectionsWithTestDocuments(
								t,
								session.Database(dbname),
								myAllNames,
							)

							withArchiveMongodump(t, func(archive string) {
								require.NoError(session.Database(dbname).Drop(ctx))

								colls, err := session.Database(dbname).
									ListCollectionNames(ctx, bson.D{})
								require.NoError(err)
								require.Empty(colls, "sanity: db drop should drop all collections")

								restore, err := getRestoreWithArgs(
									DBOption, dbname,
									ArchiveOption+"="+archive,
									"-vv",
								)
								require.NoError(err)
								defer restore.Close()

								result := restore.Restore()
								require.NoError(result.Err, "can run mongorestore")
								require.EqualValues(
									0,
									result.Failures,
									"mongorestore reports 0 failures (result=%+v)",
									result,
								)
							})

							colls, err := session.Database(dbname).
								ListCollectionNames(ctx, bson.D{})
							require.NoError(err)

							assert.ElementsMatch(t, myAllNames, colls, "all collections restored")
						},
					)
				}
			},
		)
	}

}

func createCollectionsWithTestDocuments(
	t *testing.T,
	db *mongo.Database,
	collectionNames []string,
) []*mongo.Collection {
	collections := []*mongo.Collection{}
	for _, collectionName := range collectionNames {
		collection := createCollectionWithTestDocument(t, db, collectionName)
		collections = append(collections, collection)
	}
	return collections
}

func clearDB(t *testing.T, db *mongo.Database) {
	require := require.New(t)
	collectionNames, err := db.ListCollectionNames(context.Background(), bson.D{})
	require.NoError(err, "can get collection names")
	for _, collectionName := range collectionNames {
		collection := db.Collection(collectionName)
		_, _ = collection.DeleteMany(context.Background(), bson.M{})
	}
}
