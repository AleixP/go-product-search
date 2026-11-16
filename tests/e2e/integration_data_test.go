package e2e

// The expectations of the integration test, written by hand from
// testdata/integration.tsv. They are NOT computed by the code under test: when
// the fixture changes, every literal here changes with it (see the skill
// repo-add-integration-test).

// integrationFixture is the synthetic fixture the integration test ingests.
const integrationFixture = "../../testdata/integration.tsv"

// Report literals of the ingest of the fixture with the default filters
// (--countries en:united-states, --require brand,category,nutriscore) and
// --sample-every 1 --limit 0. The fixture has 29 data rows: 25 pass and one
// each is dropped by the country, brand, category and nutriscore filter, in
// that precedence order.
const (
	wantRowsRead   = 29
	wantPassed     = 25
	wantIndexed    = 25
	wantDocCount   = 25
	wantCountry    = 1 // 9991000000101: only en:france
	wantBrand      = 1 // 9991000000103: empty brands
	wantCategory   = 1 // 9991000000102: only fr: category tags
	wantNutriscore = 1 // 9991000000104: grade "unknown"
)

// integrationDropped are the codes of the rows the ingest filters out. They
// must never come back from the API.
var integrationDropped = []string{"9991000000101", "9991000000102", "9991000000103", "9991000000104"}

// integrationDoc is one expected indexed document, as the API returns it.
type integrationDoc struct {
	id         string
	name       string
	brands     []string // as stored: trimmed, split on commas, exact duplicates removed
	categories []string // the en: tags without the prefix; other languages dropped
	grade      string   // lower case
}

// integrationDocs is derived by hand from the fixture with the ingest rules
// (cmd/ingest/record.go): a row is indexed when its countries hold
// en:united-states and it has a brand, an en: category and a grade a-e (any
// case); brands are split on commas, trimmed and deduplicated by exact text
// (case and accents kept); categories keep only the en: tags, without the
// prefix; the grade is lower-cased. The order of the table is the order of the
// file and means nothing.
var integrationDocs = []integrationDoc{
	{id: "9991000000001", name: "Dark Chocolate Bar", brands: []string{"Cocoa Crest"}, categories: []string{"chocolates", "snacks"}, grade: "c"},
	{id: "9991000000002", name: "Milk Chocolate Bar", brands: []string{"Cocoa Crest"}, categories: []string{"chocolates", "snacks"}, grade: "e"},
	{id: "9991000000003", name: "White Chocolate Chips", brands: []string{"Sweet Hearth"}, categories: []string{"chocolates", "snacks"}, grade: "d"},
	{id: "9991000000004", name: "Chocolate Hazelnut Spread", brands: []string{"Sweet Hearth"}, categories: []string{"spreads", "chocolates"}, grade: "e"},
	{id: "9991000000005", name: "Cheddar Cheese Slices", brands: []string{"Green Valley"}, categories: []string{"dairies", "cheeses", "snacks"}, grade: "c"},
	{id: "9991000000006", name: "Chili Cheese Crackers", brands: []string{"Zenith Bakery"}, categories: []string{"snacks", "biscuits", "cheeses"}, grade: "d"},
	{id: "9991000000007", name: "Chicken Noodle Soup", brands: []string{"Nordic Farms", "Green Valley"}, categories: []string{"soups"}, grade: "d"},
	{id: "9991000000008", name: "Chickpea Hummus", brands: []string{"Olive Grove"}, categories: []string{"spreads", "plant-based-foods", "snacks"}, grade: "b"},
	{id: "9991000000009", name: "Chamomile Herbal Tea", brands: []string{"Leaf House"}, categories: []string{"beverages", "plant-based-foods"}, grade: "a"},
	{id: "9991000000010", name: "Chestnut Cream", brands: []string{"Sweet Hearth"}, categories: []string{"spreads"}, grade: "e"},
	{id: "9991000000011", name: "Chewy Granola Bars", brands: []string{"Crunch Mill"}, categories: []string{"snacks", "breakfast-cereals", "biscuits"}, grade: "d"},
	{id: "9991000000012", name: "Cherry Cheesecake Yogurt", brands: []string{"Green Valley"}, categories: []string{"dairies", "cheeses", "beverages", "breakfast-cereals"}, grade: "d"},
	{id: "9991000000013", name: "Crème Fraîche", brands: []string{"Acme Foods"}, categories: []string{"dairies", "cheeses"}, grade: "b"},
	{id: "9991000000014", name: "Creme Fraiche", brands: []string{"ACME FOODS"}, categories: []string{"dairies", "beverages"}, grade: "c"},
	{id: "9991000000015", name: "CREME FRAICHE", brands: []string{"Acmé Foods"}, categories: []string{"dairies", "cheeses"}, grade: "e"},
	{id: "9991000000016", name: "Supercalifragilistic Crunch", brands: []string{"Crunch Mill"}, categories: []string{"snacks", "breakfast-cereals", "biscuits"}, grade: "e"},
	{id: "9991000000017", name: "Sparkling Orange Water", brands: []string{"Aqua Peak"}, categories: []string{"beverages", "plant-based-foods"}, grade: "b"},
	{id: "9991000000018", name: "Almond Milk Unsweetened", brands: []string{"Olive Grove"}, categories: []string{"beverages", "plant-based-foods"}, grade: "a"},
	{id: "9991000000019", name: "Whole Milk", brands: []string{"Green Valley"}, categories: []string{"dairies", "beverages"}, grade: "d"},
	{id: "9991000000020", name: "Orange Juice", brands: []string{"Aqua Peak"}, categories: []string{"beverages", "plant-based-foods"}, grade: "c"},
	{id: "9991000000021", name: "Cola Zero", brands: []string{"Fizz Co"}, categories: []string{"beverages"}, grade: "c"},
	{id: "9991000000022", name: "Tomato Pasta Sauce", brands: []string{"Nonna Pantry"}, categories: []string{"sauces", "plant-based-foods"}, grade: "d"},
	{id: "9991000000023", name: "Tomato Basil Soup", brands: []string{"Nonna Pantry"}, categories: []string{"soups", "sauces"}, grade: "e"},
	{id: "9991000000024", name: "Salted Potato Crisps", brands: []string{"Crunch Mill"}, categories: []string{"snacks"}, grade: "e"},
	{id: "9991000000025", name: "Vanilla Ice Cream", brands: []string{"Cold Peak"}, categories: []string{"frozen-desserts", "dairies", "snacks"}, grade: "e"},
}
