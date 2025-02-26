/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package types

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var datesWithTz = []struct {
	in  string
	out time.Time
}{
	{in: "2018-10-28T04:00:10Z",
		out: time.Date(2018, 10, 28, 4, 00, 10, 0, time.UTC)},
	{in: "2018-10-28T04:00:10-00:00",
		out: time.Date(2018, 10, 28, 4, 00, 10, 0, time.UTC)},
	{in: "2018-05-30T09:30:10.5Z",
		out: time.Date(2018, 5, 30, 9, 30, 10, 500000000, time.UTC)},
	{in: "2018-05-30T09:30:10.5-00:00",
		out: time.Date(2018, 5, 30, 9, 30, 10, 500000000, time.UTC)},
	{in: "2018-05-30T09:30:10-06:00",
		out: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", -6*60*60))},
	{in: "2018-05-28T14:41:57+23:00",
		out: time.Date(2018, 5, 28, 14, 41, 57, 0, time.FixedZone("", 23*60*60))},
	{in: "2018-05-28T14:41:57-23:40",
		out: time.Date(2018, 5, 28, 14, 41, 57, 0, time.FixedZone("", -23*60*60-40*60))},
	{in: "2018-05-28T14:41:57+23:59",
		out: time.Date(2018, 5, 28, 14, 41, 57, 0, time.FixedZone("", 23*60*60+59*60))},
}

var datesWithInvalidTz = []struct {
	in string
}{
	{in: "2018-05-28T14:41:57+25:00"},
	{in: "2018-05-28T14:41:57+30:00"},
	{in: "2018-05-28T14:41:57-25:01"},
	{in: "2018-05-28T14:41:57-30:00"},
}

var datesWithoutTz = []struct {
	in  string
	out time.Time
}{
	{in: "2018-10-28T04:00:10",
		out: time.Date(2018, 10, 28, 4, 00, 10, 0, time.UTC)},
	{in: "2018-05-30T09:30:10.5",
		out: time.Date(2018, 5, 30, 9, 30, 10, 500000000, time.UTC)},
	{in: "2018",
		out: time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)},
	{in: "2018-01",
		out: time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)},
	{in: "2018-01-01",
		out: time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)},
}

var invalidDates = []string{
	"abcd",
	"12345",
	"123456",
	"1234567",
	"12345678",
	"123456789",
	"1234567891",
	"11111111111111111Z",
	"111111111111111:11",
	"1111-11-11T11:11111111:1",
	"1111-11-11T11:11:1111:11",
	"18-10-28T04:00:10Z",
	"318-10-28T04:00:10",
	"2018-110-28T04:00:10",
	"20181-4-28T25:00:10",
	"2018-10-218T04:00:10",
	"2018-14-28T25:00:10",
	"2018-142-8T25:00:10",
	"2018-05-33T09:65:10.5",
	"201",
	"2018-011",
	"2018-01-011",
}

func TestTypeForName(t *testing.T) {
	for name, tid := range typeNameMap {
		typ, ok := TypeForName(name)
		require.EqualValues(t, tid, typ, "%s != %s", name, typ.Name())
		require.True(t, ok)
	}
	typ, ok := TypeForName("--invalid--")
	require.EqualValues(t, 0, typ)
	require.False(t, ok)
}

func TestValueForType(t *testing.T) {
	for name, tid := range typeNameMap {
		val := ValueForType(tid)
		require.EqualValues(t, tid, val.Tid, "%s != %s", name, val.Tid.Name())
		require.NotNil(t, val.Value)
	}
	val := ValueForType(UndefinedID)
	require.EqualValues(t, 0, val.Tid)
	require.Nil(t, val.Value)
}

func TestParseTimeWithoutTZ(t *testing.T) {
	for _, tc := range datesWithoutTz {
		out, err := ParseTime(tc.in)
		require.NoError(t, err)
		require.EqualValues(t, tc.out, out)
	}
}

func TestParseTimeWithTZ(t *testing.T) {
	var err error

	// Set local time to UTC.
	time.Local, err = time.LoadLocation("UTC")
	require.NoError(t, err)

	for _, tc := range datesWithTz {
		out, err := ParseTime(tc.in)
		require.NoError(t, err)
		require.EqualValues(t, tc.out, out)
	}
}

func TestParseTimeRejection(t *testing.T) {
	var err error

	// Set local time to UTC.
	time.Local, err = time.LoadLocation("UTC")
	require.NoError(t, err)

	for _, invalidDate := range invalidDates {
		_, err := ParseTime(invalidDate)
		require.Error(t, err)
	}
}

func TestParseTimeNonRFC3339(t *testing.T) {
	for _, tc := range datesWithInvalidTz {
		out, err := ParseTime(tc.in)
		require.Equal(t, time.Time{}, out)
		require.ErrorContains(t, err, "time zone offset hour out of range")
	}
}

func BenchmarkParseTime(b *testing.B) {
	for i := 0; i < b.N; i++ {
		for _, tc := range datesWithTz {
			_, _ = ParseTime(tc.in)
		}
		for _, tc := range datesWithoutTz {
			_, _ = ParseTime(tc.in)
		}
	}
}

func BenchmarkParseTimeRejections(b *testing.B) {
	for i := 0; i < b.N; i++ {
		for _, invalidDate := range invalidDates {
			_, _ = ParseTime(invalidDate)
		}
	}
}
