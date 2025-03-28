// Copyright (C) MongoDB, Inc. 2014-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package bsonutil

import (
	"testing"

	"github.com/mongodb/mongo-tools/common/json"
	"github.com/mongodb/mongo-tools/common/testtype"
	. "github.com/smartystreets/goconvey/convey"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestTimestampValue(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.UnitTestType)

	Convey("When converting JSON with Timestamp values", t, func() {
		testTS := primitive.Timestamp{T: 123456, I: 55}

		Convey("works for Timestamp literal", func() {

			jsonMap := map[string]interface{}{
				"ts": json.Timestamp{123456, 55},
			}

			err := ConvertLegacyExtJSONDocumentToBSON(jsonMap)
			So(err, ShouldBeNil)
			So(jsonMap["ts"], ShouldEqual, testTS)
		})

		Convey(`works for Timestamp document`, func() {
			Convey(`{"ts":{"$timestamp":{"t":123456, "i":55}}}`, func() {
				jsonMap := map[string]interface{}{
					"ts": map[string]interface{}{
						"$timestamp": map[string]interface{}{
							"t": 123456.0,
							"i": 55.0,
						},
					},
				}

				bsonMap, err := ConvertLegacyExtJSONValueToBSON(jsonMap)
				So(err, ShouldBeNil)

				realMap, ok := bsonMap.(map[string]interface{})
				So(ok, ShouldBeTrue)

				So(realMap["ts"], ShouldEqual, testTS)
			})
		})
	})
}
