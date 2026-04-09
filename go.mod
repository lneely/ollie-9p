module olliesrv

go 1.25.6

require (
	9fans.net/go v0.0.7
	ollie v0.0.0-00010101000000-000000000000
)

require gopkg.in/yaml.v3 v3.0.1 // indirect

replace ollie => ../ollie
