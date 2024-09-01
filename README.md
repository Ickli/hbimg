## About

Utility to convert an htm(l) document's URLs to base64 URIs.

## Usage

`-o <dir_path>` - select output directory. Creates one if doesn't exist. Doesn't handle path containing spaces.
`-j` - put all URIs in a new `<script>` tag.
`-c <count>` - specify count of goroutines running. Each goroutine handles one file.

### Passing files

Pass files/directories after flags. If none passed, utility looks up htm(l) files in current working directory.
