package main

var queries = []query{{
	radius: 1000,
	conditions: []condition{{
		tag: "amenity",
		values: []string{
			"bar",
			"biergarten",
			"cafe",
			"fast_food",
			"food_court",
			"fountain",
			"fuel",
			"ice_cream",
			"marketplace",
			"pub",
			"restaurant",
		},
	}},
}, {
	radius: 500,
	conditions: []condition{{
		tag: "amenity",
		values: []string{
			"bicycle_rental",
			"bicycle_repair_station",
			"bicycle_wash",
			"compressed_air",
			"place_of_worship",
			"public_bath",
			"shelter",
			"shower",
			"toilets",
		},
	}},
}, {
	radius: 200,
	conditions: []condition{{
		tag: "amenity",
		values: []string{
			"drinking_water",
			"water_point",
			"watering_place",
		},
	}},
}, {
	radius: 1000,
	conditions: []condition{{
		tag: "tourism",
		values: []string{
			"alpine_hut",
			"camp_pitch",
			"camp_site",
			"guest_house",
			"hotel",
			"hostel",
			"motel",
			"wilderness_hut",
		},
	}},
}, {
	radius: 1000,
	conditions: []condition{{
		tag:    "accommodation",
		exists: ExistsYes,
	}},
}, {
	conditions: []condition{{
		// - - tourism~"^(alpine_hut|camp_pitch|camp_site|guest_house|hostel|picnic_site|viewpoint|wilderness_hut)$"
		tag: "tourism",
		values: []string{
			"picnic_site",
			"viewpoint",
		},
	}},
}, {
	conditions: []condition{{
		// - - leisure~"^(nature_reserve|park|picnic_table|wildlife_hide)$"
		tag: "leisure",
		values: []string{
			"nature_reserve",
			"park",
			"picnic_table",
			"wildlife_hide",
		},
	}},
}, {
	conditions: []condition{{
		// - - natural~"^(spring|peak)$"
		tag: "natural",
		values: []string{
			"spring",
			"peak",
			"mountain_range",
			"ridge",
			"arete",
			"hot_spring", // OSM tags use underscores, not spaces
			"plateau",
			"saddle",
		},
	}},
}, {
	conditions: []condition{{
		//boundary=aboriginal_lands
		//boundary=national_park
		//boundary=forest
		//boundary=water_protection_area
		//boundary=protected_area
		tag: "boundary",
		values: []string{
			"protected_area",
			"aboriginal_lands",
			"national_park",
			"forest",
			"water_protection_area",
		},
	}},
}, {
	radius: 1000,
	conditions: []condition{{
		// - - man_made~"^(spring_box|water_well|water_tap)$"
		tag: "man_made",
		values: []string{
			"spring_box",
			"water_well",
			"water_tap",
			"drinking_fountain",
		},
	}},
}, {
	radius: 2000,
	conditions: []condition{{
		tag: "drinking_water",
		values: []string{
			"yes",
		},
	}},
}, {
	radius: 500,
	conditions: []condition{{
		tag:    "waterway",
		exists: ExistsYes,
	}, {
		tag: "waterway",
		notValues: []string{
			"drain",
			"dam",
			"ditch",
			"canal",
		},
	}},
}, {
	radius: 250,
	conditions: []condition{{
		tag:    "ford",
		exists: ExistsYes,
	}},
}, {
	conditions: []condition{{
		// - - place~"^(town|village|hamlet|city|neighbourhood)$"
		tag: "place",
		values: []string{
			"town",
			"village",
			"hamlet",
			"city",
			"neighbourhood",
		},
	}},
}, {
	radius: 200,
	//- - amenity="fountain"
	//  - drinking_water!="no"
	//  - drinking_water~".+"
	conditions: []condition{{
		tag:    "amenity",
		values: []string{"fountain"},
	}, {
		tag:    "drinking_water",
		exists: ExistsYes,
	}, {
		tag:       "drinking_water",
		notValues: []string{"no"},
	}},
}, {
	radius: 2000,
	conditions: []condition{{
		tag: "shop",
		values: []string{
			"bakery",
			"cheese",
			"coffee",
			"convenience",
			"dairy",
			"farm",
			"food",
			"greengrocer",
			"health_food",
			"ice_cream",
			"pastry",
			"tortilla",
			"water",
			"general",
			"kiosk",
			"supermarket",
			"chemist",
			"bicycle",
			"sports",
		},
	}},
}, {
	conditions: []condition{{
		tag:    "mountain_pass",
		values: []string{"yes"},
	}}},
}
