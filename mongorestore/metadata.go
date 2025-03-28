// Copyright (C) MongoDB, Inc. 2014-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package mongorestore

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/idx"
	"github.com/mongodb/mongo-tools/common/intents"
	"github.com/mongodb/mongo-tools/common/log"
	"github.com/mongodb/mongo-tools/common/util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Specially treated restore collection types.
const (
	Users = "users"
	Roles = "roles"
)

// struct for working with auth versions.
type authVersionPair struct {
	// Dump is the auth version of the users/roles collection files in the target dump directory
	Dump int
	// Server is the auth version of the connected MongoDB server
	Server int
}

// Metadata holds information about a collection's options and indexes.
type Metadata struct {
	Options        bson.D               `bson:"options,omitempty"`
	Indexes        []*idx.IndexDocument `bson:"indexes"`
	UUID           string               `bson:"uuid"`
	CollectionName string               `bson:"collectionName"`
}

// MetadataFromJSON takes a slice of JSON bytes and unmarshals them into usable
// collection options and indexes for restoring collections.
func (restore *MongoRestore) MetadataFromJSON(jsonBytes []byte) (*Metadata, error) {
	if len(jsonBytes) == 0 {
		// skip metadata parsing if the file is empty
		return nil, nil
	}

	meta := &Metadata{}

	err := bson.UnmarshalExtJSON(jsonBytes, true, meta)
	if err != nil {
		return nil, err
	}

	return meta, nil
}

// LoadIndexesFromBSON reads indexes from the index BSON files and
// caches them in the MongoRestore object.
func (restore *MongoRestore) LoadIndexesFromBSON() error {

	dbCollectionIndexes := make(map[string]collectionIndexes)

	for _, dbname := range restore.manager.SystemIndexDBs() {
		dbCollectionIndexes[dbname] = make(collectionIndexes)
		intent := restore.manager.SystemIndexes(dbname)
		err := intent.BSONFile.Open()
		if err != nil {
			return err
		}
		defer intent.BSONFile.Close()
		bsonSource := db.NewDecodedBSONSource(db.NewBSONSource(intent.BSONFile))
		defer bsonSource.Close()

		// iterate over stored indexes, saving all that match the collection
		for {
			indexDocument := &idx.IndexDocument{}
			if !bsonSource.Next(&indexDocument) {
				break
			}
			//nolint:errcheck
			namespace := indexDocument.Options["ns"].(string)
			dbCollectionIndexes[dbname][stripDBFromNS(namespace)] =
				append(dbCollectionIndexes[dbname][stripDBFromNS(namespace)], indexDocument)
		}
		if err := bsonSource.Err(); err != nil {
			return fmt.Errorf("error scanning system.indexes: %v", err)
		}
	}
	restore.dbCollectionIndexes = dbCollectionIndexes
	return nil
}

func stripDBFromNS(ns string) string {
	_, c := util.SplitNamespace(ns)
	return c
}

// CollectionExists returns true if the given intent's collection exists.
func (restore *MongoRestore) CollectionExists(dbName, coll string) (bool, error) {
	restore.knownCollectionsMutex.Lock()
	defer restore.knownCollectionsMutex.Unlock()

	// make sure the map exists
	if restore.knownCollections == nil {
		restore.knownCollections = map[string][]string{}
	}

	// first check if we haven't done listCollections for this database already
	if restore.knownCollections[dbName] == nil {
		// if the database name isn't in the cache, grab collection
		// names from the server
		session, err := restore.SessionProvider.GetSession()
		if err != nil {
			return false, fmt.Errorf("error establishing connection: %v", err)
		}
		collections, err := session.Database(dbName).ListCollections(context.TODO(), bson.M{})
		if err != nil {
			return false, err
		}
		// update the cache
		for collections.Next(context.TODO()) {
			colNameRaw := collections.Current.Lookup("name")
			colName, ok := colNameRaw.StringValueOK()
			if !ok {
				return false, fmt.Errorf("invalid collection name: %v", colNameRaw)
			}
			restore.knownCollections[dbName] = append(restore.knownCollections[dbName], colName)
		}
	}

	// now check the cache for the given collection name
	exists := util.StringSliceContains(restore.knownCollections[dbName], coll)
	return exists, nil
}

// addToKnownCollections will add a collection to the restore.knownCollections cache.
// This is used to update the cache after a collection has been created during a restore.
func (restore *MongoRestore) addToKnownCollections(intent *intents.Intent) {
	restore.knownCollectionsMutex.Lock()
	defer restore.knownCollectionsMutex.Unlock()

	restore.knownCollections[intent.DB] = append(restore.knownCollections[intent.DB], intent.C)
}

// CreateIndexes takes in an intent and an array of index documents and
// attempts to create them using the createIndexes command. If that command
// fails, we fall back to individual index creation.
func (restore *MongoRestore) CreateIndexes(
	dbName string,
	collectionName string,
	indexes []*idx.IndexDocument,
) error {
	// first, sanitize the indexes
	var indexNames []string
	for _, index := range indexes {
		// update the namespace of the index before inserting
		index.Options["ns"] = dbName + "." + collectionName

		// check for length violations before building the command
		if restore.serverVersion.LT(db.Version{4, 2, 0}) {
			fullIndexName := fmt.Sprintf("%v.$%v", index.Options["ns"], index.Options["name"])
			if len(fullIndexName) > 127 {
				return fmt.Errorf(
					"cannot restore index with namespace '%v': "+
						"namespace is too long (max size is 127 bytes)", fullIndexName)
			}
		}

		nameStr, isString := index.Options["name"].(string)
		if !isString {
			return fmt.Errorf(
				"expected name (%T: %v) to be a string",
				index.Options["name"],
				index.Options["name"],
			)
		}

		indexNames = append(indexNames, nameStr)

		// remove the index version, forcing an update,
		// unless we specifically want to keep it
		if !restore.OutputOptions.KeepIndexVersion {
			delete(index.Options, "v")
		}
	}

	session, err := restore.SessionProvider.GetSession()
	if err != nil {
		return fmt.Errorf("error establishing connection: %v", err)
	}

	// then attempt the createIndexes command
	rawCommand := bson.D{
		{"createIndexes", collectionName},
		{"indexes", indexes},
	}

	log.Logvf(
		log.Info,
		"\trun create Index command for indexes: %v",
		strings.Join(indexNames, ", "),
	)

	if restore.serverVersion.GTE(db.Version{4, 1, 9}) {
		rawCommand = append(rawCommand, bson.E{"ignoreUnknownIndexOptions", true})
	}

	err = session.Database(dbName).RunCommand(context.TODO(), rawCommand).Err()
	if err == nil {
		return nil
	}
	if err.Error() != "no such cmd: createIndexes" {
		return fmt.Errorf("createIndex error: %v", err)
	}

	// if we're here, the connected server does not support the command, so we fall back
	log.Logv(log.Info, "\tcreateIndexes command not supported, attemping legacy index insertion")
	for _, idx := range indexes {
		log.Logvf(log.Info, "\tmanually creating index %v", idx.Options["name"])
		err = restore.LegacyInsertIndex(dbName, idx)
		if err != nil {
			return fmt.Errorf("error creating index %v: %v", idx.Options["name"], err)
		}
	}
	return nil
}

// LegacyInsertIndex takes in an intent and an index document and attempts to
// create the index on the "system.indexes" collection.
func (restore *MongoRestore) LegacyInsertIndex(dbName string, index *idx.IndexDocument) error {
	session, err := restore.SessionProvider.GetSession()
	if err != nil {
		return fmt.Errorf("error establishing connection: %v", err)
	}

	indexCollection := session.Database(dbName).Collection("system.indexes")
	_, err = indexCollection.InsertOne(context.TODO(), index)
	if err != nil {
		return fmt.Errorf("insert error: %v", err)
	}

	return nil
}

// CreateCollection creates the collection specified in the intent with the
// given options.
func (restore *MongoRestore) CreateCollection(
	intent *intents.Intent,
	options bson.D,
	uuid string,
) error {
	session, err := restore.SessionProvider.GetSession()
	if err != nil {
		return fmt.Errorf("error establishing connection: %v", err)
	}

	switch {

	case uuid != "":
		return restore.createCollectionWithApplyOps(session, intent, options, uuid)
	default:
		return restore.createCollectionWithCommand(session, intent, options)
	}

}

// UpdateAutoIndexId updates {autoIndexId: false} to {autoIndexId: true} if the server version is
// >= 4.0 and the database is not `local`.
func (restore *MongoRestore) UpdateAutoIndexId(options bson.D) {
	if restore.serverVersion.GTE(db.Version{4, 0, 0}) {
		for i, elem := range options {
			if elem.Key == "autoIndexId" && elem.Value == false &&
				restore.ToolOptions.Namespace.DB != "local" {
				options[i].Value = true
				log.Logvf(
					log.Always,
					"{autoIndexId: false} is not allowed in server versions >= 4.0. Changing to {autoIndexId: true}.",
				)
			}
		}
	}
}

func (restore *MongoRestore) createCollectionWithCommand(
	session *mongo.Client,
	intent *intents.Intent,
	options bson.D,
) error {
	restore.UpdateAutoIndexId(options)

	command := createCollectionCommand(intent, options)

	// If there is no error, the result doesnt matter
	singleRes := session.Database(intent.DB).RunCommand(context.TODO(), command, nil)
	if err := singleRes.Err(); err != nil {
		return fmt.Errorf("error running create command: %v", err)
	}

	res := bson.M{}
	if err := singleRes.Decode(&res); err != nil {
		return fmt.Errorf("error decoding result of create command: %v", err)
	}
	if util.IsFalsy(res["ok"]) {
		return fmt.Errorf("create command: %v", res["errmsg"])
	}
	return nil

}

func (restore *MongoRestore) createCollectionWithApplyOps(
	session *mongo.Client,
	intent *intents.Intent,
	options bson.D,
	uuidHex string,
) error {
	restore.UpdateAutoIndexId(options)

	command := createCollectionCommand(intent, options)
	uuid, err := hex.DecodeString(uuidHex)
	if err != nil {
		return fmt.Errorf("Couldn't restore UUID because UUID was invalid: %s", err)
	}

	createOp := struct {
		Operation string            `bson:"op"`
		Namespace string            `bson:"ns"`
		Object    bson.D            `bson:"o"`
		UI        *primitive.Binary `bson:"ui,omitempty"`
	}{
		Operation: "c",
		Namespace: intent.DB + ".$cmd",
		Object:    command,
		UI:        &primitive.Binary{Subtype: 0x04, Data: uuid},
	}

	return restore.ApplyOps(session, []interface{}{createOp})
}

func createCollectionCommand(intent *intents.Intent, options bson.D) bson.D {
	return append(bson.D{{"create", intent.C}}, options...)
}

// RestoreUsersOrRoles accepts a users intent and a roles intent, and restores
// them via _mergeAuthzCollections. Either or both can be nil. In the latter case
// nothing is done.
//
// _mergeAuthzCollections is an internal server command implemented specifically for mongorestore. Instead of inserting
// into the admin.system.{roles, users} collections (which isn't allowed due to some locking policies), we construct
// temporary collections that are then merged with or replace the existing ones.
//
// The "drop" argument that determines whether the merge replaces the existing users/roles or adds to them.
//
// The "db" argument determines which databases' users and roles are merged. If left blank, it merges users and roles
// from all databases. When the user restores the admin database, we assume they wish to restore the users and roles for
// all databases, not just the admin ones, so we leave the "db" field blank in that case.
//
// The "temp{Users,Roles}Collection" arguments determine which temporary collections to merge from, and the presence of
// either determines which collection to merge to. (e.g. if tempUsersCollection is defined, admin.system.users is merged
// into).
//
// This command must be run on the "admin" database. Thus, the temporary collections must be on the admin db as well.
// This command must also be run on the primary.
//
// Example command:
//
//	{
//	   _mergeAuthzCollections: 1,
//	   db: "foo",
//	   tempUsersCollection: "myTempUsers"
//	   drop: true
//	   writeConcern: {w: "majority"}
//	}
func (restore *MongoRestore) RestoreUsersOrRoles(users, roles *intents.Intent) error {

	type loopArg struct {
		intent             *intents.Intent
		intentType         string
		mergeParamName     string
		tempCollectionName string
	}

	if users == nil && roles == nil {
		return nil
	}

	if users != nil && roles != nil && users.DB != roles.DB {
		return fmt.Errorf(
			"can't restore users and roles to different databases, %v and %v",
			users.DB,
			roles.DB,
		)
	}

	args := []loopArg{}
	mergeArgs := bson.D{}
	userTargetDB := ""

	if users != nil {
		args = append(
			args,
			loopArg{users, "users", "tempUsersCollection", restore.OutputOptions.TempUsersColl},
		)
	}
	if roles != nil {
		args = append(
			args,
			loopArg{roles, "roles", "tempRolesCollection", restore.OutputOptions.TempRolesColl},
		)
	}

	session, err := restore.SessionProvider.GetSession()
	if err != nil {
		return fmt.Errorf("error establishing connection: %v", err)
	}

	// For each of the users and roles intents:
	//   build up the mergeArgs component of the _mergeAuthzCollections command
	//   upload the BSONFile to a temporary collection
	for _, arg := range args {

		if arg.intent.Size == 0 {
			// MongoDB complains if we try and remove a non-existent collection, so we should
			// just skip auth collections with empty .bson files to avoid gnarly logic later on.
			log.Logvf(
				log.Always,
				"%v file '%v' is empty; skipping %v restoration",
				arg.intentType,
				arg.intent.Location,
				arg.intentType,
			)
		}
		log.Logvf(log.Always, "restoring %v from %v", arg.intentType, arg.intent.Location)

		mergeArgs = append(mergeArgs, bson.E{
			Key:   arg.mergeParamName,
			Value: "admin." + arg.tempCollectionName,
		})

		err := arg.intent.BSONFile.Open()
		if err != nil {
			return err
		}
		defer arg.intent.BSONFile.Close()
		bsonSource := db.NewDecodedBSONSource(db.NewBSONSource(arg.intent.BSONFile))
		defer bsonSource.Close()

		tempCollectionNameExists, err := restore.CollectionExists("admin", arg.tempCollectionName)
		if err != nil {
			return err
		}
		if tempCollectionNameExists {
			log.Logvf(
				log.Info,
				"dropping preexisting temporary collection admin.%v",
				arg.tempCollectionName,
			)
			err = session.Database("admin").Collection(arg.tempCollectionName).Drop(context.TODO())
			if err != nil {
				return fmt.Errorf(
					"error dropping preexisting temporary collection %v: %v",
					arg.tempCollectionName,
					err,
				)
			}
		}

		log.Logvf(log.DebugLow, "restoring %v to temporary collection", arg.intentType)
		result := restore.RestoreCollectionToDB(
			"admin",
			arg.tempCollectionName,
			bsonSource,
			arg.intent.BSONFile,
			0,
			"",
		)
		if result.Err != nil {
			return fmt.Errorf("error restoring %v: %v", arg.intentType, result.Err)
		}

		// make sure we always drop the temporary collection
		defer func(cleanupArg loopArg) {
			session, e := restore.SessionProvider.GetSession()
			if e != nil {
				// logging errors here because this has no way of returning that doesn't mask other errors
				log.Logvf(
					log.Info,
					"error establishing connection to drop temporary collection admin.%v: %v",
					cleanupArg.tempCollectionName,
					e,
				)
				return
			}
			log.Logvf(
				log.DebugHigh,
				"dropping temporary collection admin.%v",
				cleanupArg.tempCollectionName,
			)
			e = session.Database("admin").
				Collection(cleanupArg.tempCollectionName).
				Drop(context.TODO())
			if e != nil {
				log.Logvf(
					log.Info,
					"error dropping temporary collection admin.%v: %v",
					cleanupArg.tempCollectionName,
					e,
				)
			}
		}(arg)
		userTargetDB = arg.intent.DB
	}

	if userTargetDB == "admin" {
		// _mergeAuthzCollections uses an empty db string as a sentinel for "all databases"
		userTargetDB = ""
	}

	adminDB := session.Database("admin")

	command := bson.D{}
	command = append(command,
		bson.E{Key: "_mergeAuthzCollections", Value: 1})
	command = append(command,
		mergeArgs...)
	command = append(command,
		bson.E{Key: "drop", Value: restore.OutputOptions.Drop},
		bson.E{Key: "db", Value: userTargetDB})

	if restore.ToolOptions.WriteConcern != nil {
		_, wcBson, err := restore.ToolOptions.WriteConcern.MarshalBSONValue()
		if err != nil {
			return fmt.Errorf("error parsing write concern: %v", err)
		}

		writeConcern := bson.M{}
		err = bson.Unmarshal(wcBson, &writeConcern)
		if err != nil {
			return fmt.Errorf("error parsing write concern: %v", err)
		}

		command = append(command, bson.E{Key: "writeConcern", Value: writeConcern})
	}

	log.Logvf(log.DebugLow, "merging users/roles from temp collections")
	resSingle := adminDB.RunCommand(context.TODO(), command)
	if err = resSingle.Err(); err != nil {
		return fmt.Errorf("error running merge command: %v", err)
	}
	res := bson.M{}
	if err = resSingle.Decode(&res); err != nil {
		return fmt.Errorf("error decoding result of merge command: %v", err)
	}
	if util.IsFalsy(res["ok"]) {
		return fmt.Errorf("_mergeAuthzCollections command: %v", res["errmsg"])
	}
	return nil
}

// GetDumpAuthVersion reads the admin.system.version collection in the dump directory
// to determine the authentication version of the files in the dump. If that collection is not
// present in the dump, we try to infer the authentication version based on its absence.
// Returns the authentication version number and any errors that occur.
func (restore *MongoRestore) GetDumpAuthVersion() (int, error) {
	// authSchema doc has been removed from system.version from 8.1+ (SERVER-83663)
	// The only auth version used by server 8.1+ is 5
	if restore.dumpServerVersion.GTE(db.Version{8, 1, 0}) {
		log.Logvf(
			log.DebugLow,
			"skipping authSchema check for server version %v",
			restore.dumpServerVersion,
		)
		return 5, nil
	}

	// first handle the case where we have no auth version
	intent := restore.manager.AuthVersion()
	if intent == nil {
		if restore.InputOptions.RestoreDBUsersAndRoles {
			// If we are using --restoreDbUsersAndRoles, we cannot guarantee an
			// $admin.system.version collection from a 2.6 server,
			// so we can assume up to version 3.
			log.Logvf(
				log.Always,
				"no system.version bson file found in '%v' database dump",
				restore.ToolOptions.Namespace.DB,
			)
			log.Logv(
				log.Always,
				"warning: assuming users and roles collections are of auth version 3",
			)
			log.Logv(
				log.Always,
				"if users are from an earlier version of MongoDB, they may not restore properly",
			)
			return 3, nil
		}
		log.Logv(log.Info, "no system.version bson file found in dump")
		log.Logv(
			log.Always,
			"assuming users in the dump directory are from <= 2.4 (auth version 1)",
		)
		return 1, nil
	}

	err := intent.BSONFile.Open()
	if err != nil {
		return 0, err
	}
	defer intent.BSONFile.Close()
	bsonSource := db.NewDecodedBSONSource(db.NewBSONSource(intent.BSONFile))
	defer bsonSource.Close()

	for {
		versionDoc := bson.M{}
		if !bsonSource.Next(&versionDoc) {
			break
		}

		id, ok := versionDoc["_id"].(string)
		if ok && id == "authSchema" {
			switch authVersion := versionDoc["currentVersion"].(type) {
			case int:
				return authVersion, nil
			case int32:
				return int(authVersion), nil
			case int64:
				return int(authVersion), nil
			default:
				return 0, fmt.Errorf("can't unmarshal system.version curentVersion as an int: %v", versionDoc["currentVersion"])
			}
		}
		log.Logvf(
			log.DebugLow,
			"system.version document is not an authSchema %v",
			versionDoc["_id"],
		)
	}
	err = bsonSource.Err()
	if err != nil {
		log.Logvf(log.Info, "can't unmarshal system.version document: %v", err)
	}
	return 0, fmt.Errorf("system.version bson file does not have authSchema document")
}

// ValidateAuthVersions compares the authentication version of the dump files and the
// authentication version of the target server, and returns an error if the versions
// are incompatible.
func (restore *MongoRestore) ValidateAuthVersions() error {
	if restore.authVersions.Dump == 2 || restore.authVersions.Dump == 4 {
		return fmt.Errorf(
			"cannot restore users and roles from a dump file with auth version %v; "+
				"finish the upgrade or roll it back", restore.authVersions.Dump)
	}
	if restore.authVersions.Server == 2 || restore.authVersions.Server == 4 {
		return fmt.Errorf(
			"cannot restore users and roles to a server with auth version %v; "+
				"finish the upgrade or roll it back", restore.authVersions.Server)
	}
	switch restore.authVersions {
	case authVersionPair{3, 5}:
		log.Logv(log.Info,
			"restoring users and roles of auth version 3 to a server of auth version 5")
	case authVersionPair{5, 5}:
		log.Logv(log.Info,
			"restoring users and roles of auth version 5 to a server of auth version 5")
	case authVersionPair{3, 3}:
		log.Logv(log.Info,
			"restoring users and roles of auth version 3 to a server of auth version 3")
	case authVersionPair{1, 1}:
		log.Logv(log.Info,
			"restoring users and roles of auth version 1 to a server of auth version 1")
	case authVersionPair{1, 5}:
		return fmt.Errorf("cannot restore users of auth version 1 to a server of auth version 5")
	case authVersionPair{5, 3}:
		return fmt.Errorf("cannot restore users of auth version 5 to a server of auth version 3")
	case authVersionPair{1, 3}:
		log.Logv(log.Info,
			"restoring users and roles of auth version 1 to a server of auth version 3")
		log.Logv(log.Always,
			"users and roles will have to be updated with the authSchemaUpgrade command")
	case authVersionPair{5, 1}:
		fallthrough
	case authVersionPair{3, 1}:
		return fmt.Errorf(
			"cannot restore users and roles dump file >= auth version 3 to a server of auth version 1",
		)
	default:
		return fmt.Errorf("invalid auth pair: dump=%v, server=%v",
			restore.authVersions.Dump, restore.authVersions.Server)
	}
	return nil

}

// ShouldRestoreUsersAndRoles returns true if mongorestore should go through
// the process of restoring collections pertaining to authentication.
func (restore *MongoRestore) ShouldRestoreUsersAndRoles() bool {
	if restore.SkipUsersAndRoles {
		return false
	}

	// If the user has done anything that would indicate the restoration
	// of users and roles (i.e. used --restoreDbUsersAndRoles, -d admin, or
	// is doing a full restore), and the tool isn't connected to an atlas proxy
	// then we check if users or roles BSON files actually exist in the dump
	// dir. If they do, return true.
	if (restore.InputOptions.RestoreDBUsersAndRoles ||
		restore.ToolOptions.Namespace.DB == "" ||
		restore.ToolOptions.Namespace.DB == "admin") &&
		!restore.isAtlasProxy {
		if restore.manager.Users() != nil || restore.manager.Roles() != nil {
			return true
		}
	}
	return false
}

// DropCollection drops the intent's collection.
func (restore *MongoRestore) DropCollection(intent *intents.Intent) error {
	session, err := restore.SessionProvider.GetSession()
	if err != nil {
		return fmt.Errorf("error establishing connection: %v", err)
	}
	err = session.Database(intent.DB).Collection(intent.C).Drop(context.TODO())
	if err != nil {
		return fmt.Errorf("error dropping collection: %v", err)
	}
	return nil
}

// EnableMixedSchemaInTimeseriesBucket runs collMod to turn on timeseriesBucketsMayHaveMixedSchemaData
// for a timeseries collection.
func (restore *MongoRestore) EnableMixedSchemaInTimeseriesBucket(dbName, colName string) error {
	session, err := restore.SessionProvider.GetSession()
	if err != nil {
		return fmt.Errorf("error establishing connection: %v", err)
	}

	return session.Database(dbName).RunCommand(context.Background(), bson.D{
		{"collMod", colName},
		{"timeseriesBucketsMayHaveMixedSchemaData", true},
	}).Err()
}
