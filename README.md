# errchk

errchk is a program for checking for unchecked errors in go programs.

This is a modified version of https://github.com/kisielk/errcheck with the following changes:

- Does not traverse vendor directories
- Gives a reason why each unchecked error message was produced
- Output is now PATH:LINE:COLUMN: MESSAGE - SOURCE
- Renamed to allow both to be installed in $GOPATH/bin/

## Install

    go get -u github.com/richardwilkes/errchk

errchk requires Go 1.6 or newer and depends on the package go/loader from the golang.org/x/tools repository.

## Use

For basic usage, just give the package path of interest as the first argument:

    errchk github.com/richardwilkes/errchk/testdata

To check all packages beneath the current directory:

    errchk ./...

Or check all packages in your $GOPATH and $GOROOT:

    errchk all

errchk also recognizes the following command-line options:

The `-tags` flag takes a space-separated list of build tags, just like `go
build`. If you are using any custom build tags in your code base, you may need
to specify the relevant tags here.

The `-asserts` flag enables checking for ignored type assertion results. It
takes no arguments.

The `-blank` flag enables checking for assignments of errors to the
blank identifier. It takes no arguments.


## Excluding functions

Use the `-exclude` flag to specify a path to a file containing a list of functions to
be excluded.

    errchk -exclude errchk_excludes.txt path/to/package

The file should contain one function signature per line. The format for function signatures is
`package.FunctionName` while for methods it's `(package.Receiver).MethodName` for value receivers
and `(*package.Receiver).MethodName` for pointer receivers.

An example of an exclude file is:

    io/ioutil.ReadFile
    (*net/http.Client).Do

The exclude list is combined with an internal list for functions in the Go standard library that
have an error return type but are documented to never return an error.


### The deprecated method

The `-ignore` flag takes a comma-separated list of pairs of the form package:regex.
For each package, the regex describes which functions to ignore within that package.
The package may be omitted to have the regex apply to all packages.

For example, you may wish to ignore common operations like Read and Write:

    errchk -ignore '[rR]ead|[wW]rite' path/to/package

or you may wish to ignore common functions like the `print` variants in `fmt`:

    errchk -ignore 'fmt:[FS]?[Pp]rint*' path/to/package

The `-ignorepkg` flag takes a comma-separated list of package import paths
to ignore:

    errchk -ignorepkg 'fmt,encoding/binary' path/to/package

Note that this is equivalent to:

    errchk -ignore 'fmt:.*,encoding/binary:.*' path/to/package

If a regex is provided for a package `pkg` via `-ignore`, and `pkg` also appears
in the list of packages passed to `-ignorepkg`, the latter takes precedence;
that is, all functions within `pkg` will be ignored.

Note that by default the `fmt` package is ignored entirely, unless a regex is
specified for it. To disable this, specify a regex that matches nothing:

    errchk -ignore 'fmt:a^' path/to/package

The `-ignoretests` flag disables checking of `_test.go` files. It takes
no arguments.

## Cgo

Currently errchk is unable to check packages that `import "C"` due to limitations
in the importer.

However, you can use errchk on packages that depend on those which use cgo. In
order for this to work you need to `go install` the cgo dependencies before running
errchk on the dependant packages.

See https://github.com/kisielk/errcheck/issues/16 for more details.

## Exit Codes

errchk returns 1 if any problems were found in the checked files.
It returns 2 if there were any other failures.
