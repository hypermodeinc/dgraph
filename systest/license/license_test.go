//go:build integration || upgrade

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
)

var expiredKey = []byte(`-----BEGIN PGP MESSAGE-----

owEBwgI9/ZANAwAKAXomeeH3SyppAax7YgxsaWNlbnNlLmpzb25etF5+ewogICJ1
c2VyIjogIkRncmFwaCBUZXN0IEtleSIsCiAgIm1heF9ub2RlcyI6IDE4NDQ2NzQ0
MDczNzA5NTUxNjE1LAogICJleHBpcnkiOiAiMTk3MC0wMS0wMVQwMDowMDowMFoi
Cn0KiQIzBAABCgAdFiEED3lYS97wtaMT1MW+eiZ54fdLKmkFAl60Xn4ACgkQeiZ5
4fdLKmlVYQ//afX0H7Seb0ukxCMAcM8uqlBEGCEFv3K34avk1g8XRa6y4q/Ys0uU
DSaaDWdQ8IS5Q9SNlZBbJuqO6Pf1R01dEPTYQizWkDjYIBsY9xJnMZKEaA+F3bkn
8TXqI588+AvbqxHosz8cvh/nG+Ajk451rI9c2bqKB/FvH/zI6XyfUjqN+PvrqH0E
POA7nqSrWDemW4cMgNR4PhXehB/n2i3G6cPpwgCUd+N00N1f1mir/LmL6G5T4PrG
BmVz9fOdEr+U85PbMF9vOke9LkLQYdnF1hEV+7++t2/uoaLDYbxYhUnXpJZxwCBX
DQTievpyQF47HzuifvqUyxDSEsYiSGhhap1e/tvf1VaZoFUuTYQQpiV7+9K3UrL0
SnJ5TRWS7cEKBLyZszrakGpqVakvEUlWO4wG0Fp4kUa4YXw8t58oqHRn9aAtoqJd
UOLnq2semUttaySR4DHhjneO3/RoVm79/aaqMi/QNJzc9Tt9nY0AgcYlA3bVXmAZ
nM9Rgi6SaO5DxnRdhFzZyYQMb4onFDI0eYMOhPm+NmKWplkFXB+mKPKj5o/pcEb4
SWHt8fUAWDLsmcooIixDmSay14aBmF08hQ1vtJkY7/jo3hlK36GrLnNdN4IODqk/
I8mUd/jcj3NZtGWFoxKq4laK/ruoeoHnWMznJyMm75nzcU5QZU9yEEI=
=2lFm
-----END PGP MESSAGE-----
`)

var invalidKey = []byte(`-----BEGIN PGP MESSAGE-----

x7YgxsaWNlbnNlLmpzb25etF5owEBwgI9omeeH3SyppAa/ZANAwAKAX+ewogICJ1
c2VyIjogIkRncmFwaCBUZXN0IEtleSIsCiAgIm1heF9ub2RlcyI6IDE4NDQ2NzQ0
MDczNzA5NTUxNjE1LAogICJleHBpcnkiOiAiMTk3MC0wMS0wMVQwMDowMDowMFoi
Cn0KiQIzBAABCgAdFiEED3lYS97wtaMT1MW+eiZ54fdLKmkFAl60Xn4ACgkQeiZ5
4fdLKmlVYQ//afX0H7Seb0ukxCMAcM8uqlBEGCEFv3K34avk1g8XRa6y4q/Ys0uU
DSaaDWdQ8QizWkDjYIBsY9xJnMZKEaAIS5Q9SNlZBbJuqO6Pf1R01dEPTY+F3bkn
8T1rI9c2bqKB/FvH/zI6XXqI588+AvbqxHosz8cvh/nG+Ajk45yfUjqN+PvrqH0E
POA7nqSrWDemW4cMgNR4PhXehB/n2i3G6cPpwgCUd+N00N1f1mir/LmL6G5T4PrG
BmVz9fOdEr+U85PbMF9vOke9LkLQYdnF1hEV+7++t2/uoaLDYbxYhUnXpJZxwCBX
DQTievpyQxDSEsYiSGhhap1e/tvf1VaZoFUuTYQQpiV7F47HzuifvqUy+9K3UrL0
SnJ5TRWS7cEKBLyZszrakGpqVakvEUlWO4wG0Fp4kUa4YXw8t58oqHRn9aAtoqJd
UOLnq2semUttaySR4DHhjneO3/RoVm79/aaqMi/QNJzc9Tt9nY0AgcYlA3bVXmAZ
nM9Rgi6SaO5DxnRdhFzZyYQMb4onFDI0eYMOhPm+NmKWplkFXB+mKPKj5o/pcEb4
SWHt8fUAWDLsmcJkY7/jo3hlK36GrLnNdN4IODqkooIixDmSay14aBmF08hQ1vt/
I8jcj3NZtGWFoxKq4laK/ruoeoHnWMznJyMm7mUd/5nzcU5QZU9yEEI=
=2lFm
-----END PGP MESSAGE-----
`)

type testInp struct {
	name       string
	licenseKey []byte
	code       string
	user       string
	message    string
}

var tests = []testInp{
	{
		"Using expired entrerprise license key, should be able to extract user information",
		expiredKey,
		`Success`,
		`Dgraph Test Key`,
		``,
	},
	{
		"Using invalid entrerprise license key should return an error",
		invalidKey,
		``,
		``,
		`while extracting enterprise details from the license: while reading PGP message from license file: openpgp: unsupported feature: public key version`, //nolint:lll
	},
	{
		"Using empty entrerprise license key should return an error",
		[]byte(``),
		``,
		``,
		`while extracting enterprise details from the license: while decoding license file: EOF`,
	},
}

func (lsuite *LicenseTestSuite) TestEnterpriseLicenseWithHttpEndPoint() {
	// run this test using the http endpoint

	t := lsuite.T()

	hcli, err := lsuite.dc.HTTPClient()
	require.NoError(t, err)

	for _, tt := range tests {
		enterpriseResponse, err := hcli.ApplyLicenseHTTP(tt.licenseKey)

		if err == nil && enterpriseResponse.Code == `Success` {
			// Check if the license is applied
			require.Equal(t, enterpriseResponse.Code, tt.code)

			// check the user information in case the license is applied
			// Expired license should not be enabled even after it is applied
			assertLicenseNotEnabled(t, hcli, tt.user)
		} else {
			// check the error message in case the license is not applied
			require.Nil(t, enterpriseResponse)
		}
	}
}

func (lsuite *LicenseTestSuite) TestEnterpriseLicenseWithGraphqlEndPoint() {
	// this time, run them using the GraphQL admin endpoint

	t := lsuite.T()
	hcli, err := lsuite.dc.HTTPClient()
	require.NoError(t, err)

	for _, tt := range tests {
		resp, err := hcli.ApplyLicenseGraphQL(tt.licenseKey)

		if tt.code == `Success` {
			require.NoError(t, err)
			// Check if the license is applied
			dgraphapi.CompareJSON(`{"enterpriseLicense":{"response":{"code":"Success"}}}`, string(resp))

			// check the user information in case the license is applied
			// Expired license should not be enabled even after it is applied
			assertLicenseNotEnabled(t, hcli, tt.user)
		} else {
			dgraphapi.CompareJSON(`{"enterpriseLicense":null}`, string(resp))
			// check the error message in case the license is not applied
			require.Contains(t, err.Error(), tt.message)
		}
	}
}

func assertLicenseNotEnabled(t *testing.T, hcli *dgraphapi.HTTPClient, user string) {
	response, err := hcli.GetLicenseInfo()
	require.NoError(t, err)

	require.Equal(t, response.Extensions["user"], user)
	require.Equal(t, response.Extensions["enabled"], false)
}
