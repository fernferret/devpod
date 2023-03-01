# envy

Automatically load flags as environment variables with pflag!

## Install

```console
go install github.com/fernferret/envy
```

## About

There are a lot of solutions for loaing environment variables, but I find a lot
of them very heavy/confusing. All I wanted was:

* Something to crawl the flags I'd defined and read from a `PFX_` + `flag`
* A way to define exclusions for desstructive flags that **must** be set
  explicitly
* A way to define exclusions for "well known" variables like `$KUBECONFIG`
* Don't mangle the default value in the flag help if an envvar was set

Enter: envy. The basic idea and name came from
https://github.com/jamiealquiza/envy, however this version requires using cobra
which I find a bit overkill for some of my small apps. It also didn't support
some of the extended features like `Disable` and `SetEnvName`.

The order of values is kept very simple:

1. Flag value
2. Environment value
3. Default flag value

This means the flag value, like `--foo=x`, will always take priority. If it's
not set, then the environment variable will be checked, and if that's not set
envy will use the default flag value.

## Usage

Here's a really simple use case, with more examples in the documentation.

```go
package main

import (
    "fmt"

    "github.com/fernferret/envy"
    "github.com/spf13/pflag"
)

func main() {
    url := pflag.String("url", "http://localhost:8080", "set the url")
    once := pflag.Bool("once", false, "only run processing once")

    // Don't use FOO_URL, instead use MY_HTTP_URL as the env var
    envy.SetEnvName("url", "MY_HTTP_URL")

    // Parse items with a prefix of MYAPP_
    envy.Parse("MYAPP")

    pflag.Parse()

    fmt.Printf("url was %s\n", *url)
    fmt.Printf("once was %v\n", *once)
}
```

```console

# Environment defaults show up in brackets, these are unset
./foo -h
Usage of ./foo:
      --once         only run processing once [MYAPP_ONCE]
      --url string   set the url [MY_HTTP_URL] (default "http://localhost:8080")
pflag: help requested

./foo --once
url was http://localhost:8080
once was true

# Now let's set both envvars, booleans are set with just "true" or "false"
export MYAPP_ONCE true
export MY_HTTP_URL https://www.google.com

# Environment defaults show up inside the brackets if set at runtime
./foo -h
Usage of ./foo:
      --once         only run processing once [MYAPP_ONCE true]
      --url string   set the url [MY_HTTP_URL https://www.google.com] (default "http://localhost:8080")
pflag: help requested

# Load both values from envvars
./foo
url was https://www.google.com
once was true

# Flags *always* take priority
./foo --once=false
url was https://www.google.com
once was false
```
