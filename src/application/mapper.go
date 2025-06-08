package application

// Mapper maps Source type to Destination type
type Mapper[Source any, Destination any] interface {
	Map(Source) Destination
	MapArray([]Source) []Destination
}
