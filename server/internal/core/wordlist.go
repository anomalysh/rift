package core

// adjectives and nouns produce friendly, memorable subdomains in the style of
// Heroku and Docker (e.g. "swift-otter-42"). Every entry is lowercase ASCII
// with no hyphen, so any adjective-noun pair satisfies the default subdomain
// pattern. The lists are curated to be inoffensive and unambiguous when read
// aloud or typed; nothing here should embarrass a user who is handed one at
// random.
//
// Collision resistance does not rest on these lists alone: the store's unique
// index is the real arbiter, and the generator retries on conflict. The
// numeric suffix and the list sizes only make a first-try collision rare.
var adjectives = []string{
	"amber", "arctic", "azure", "bold", "brave", "breezy", "bright", "brisk",
	"calm", "candid", "cheery", "civic", "classic", "clever", "cobalt", "cosmic",
	"cozy", "crisp", "curious", "daring", "dawn", "deft", "dewy", "dreamy",
	"eager", "easy", "electric", "elegant", "ember", "epic", "fair", "fancy",
	"fearless", "fleet", "fluent", "fond", "frosty", "gentle", "giddy", "glad",
	"gleaming", "golden", "graceful", "grand", "hardy", "hazel", "hearty", "honest",
	"humble", "ideal", "indigo", "ivory", "jade", "jaunty", "jolly", "jovial",
	"keen", "kind", "lively", "lucky", "lunar", "lush", "mellow", "merry",
	"mighty", "mint", "misty", "modest", "noble", "nimble", "olive", "opal",
	"pastel", "patient", "peppy", "placid", "plucky", "polar", "prime", "proud",
	"quaint", "quick", "quiet", "radiant", "rapid", "rare", "regal", "robust",
	"rosy", "royal", "ruby", "rustic", "sage", "sandy", "scarlet", "serene",
	"sharp", "shiny", "silent", "silver", "sleek", "smart", "smooth", "snappy",
	"solar", "spry", "stellar", "sturdy", "sunny", "svelte", "swift", "teal",
	"tidy", "trusty", "upbeat", "urban", "valiant", "velvet", "vivid", "warm",
	"whimsical", "wild", "winter", "wise", "witty", "woven", "zesty", "zippy",
}

var nouns = []string{
	"acorn", "alcove", "anchor", "arbor", "aspen", "atlas", "aurora", "badger",
	"beacon", "birch", "bison", "bloom", "brook", "canyon", "cedar", "cinder",
	"cloud", "clover", "comet", "coral", "cove", "crane", "crest", "delta",
	"dune", "eagle", "ember", "falcon", "fern", "finch", "fjord", "flint",
	"forest", "fox", "galaxy", "garden", "geode", "glade", "glacier", "grove",
	"harbor", "hawk", "hazel", "heron", "hollow", "horizon", "island", "ivy",
	"jasper", "jetty", "koala", "lagoon", "lantern", "ledge", "lily", "lynx",
	"maple", "marble", "meadow", "mesa", "meteor", "moss", "nebula", "oak",
	"oasis", "ocelot", "orbit", "otter", "owl", "panda", "pebble", "petal",
	"pine", "planet", "plateau", "pond", "poplar", "prairie", "quail", "quartz",
	"quill", "rabbit", "raven", "reef", "ridge", "river", "robin", "sable",
	"sequoia", "shore", "sky", "slate", "sparrow", "spring", "spruce", "star",
	"stone", "stork", "stream", "summit", "swan", "thicket", "thistle", "thrush",
	"tide", "timber", "topaz", "trail", "tulip", "tundra", "vale", "valley",
	"vireo", "vista", "willow", "wisp", "wolf", "woods", "wren", "zephyr",
}
