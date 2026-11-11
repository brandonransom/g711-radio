// Geographic Area Coordination Centers, in the order the landing page lists
// them. Streams reference a GACC by its code in their "gacc" tag.
window.GACCS = [
  { code: "AICC", name: "Alaska Interagency Coordination Center", location: "Fort Wainwright, AK" },
  { code: "EACC", name: "Eastern Area Coordination Center", location: "Milwaukee, WI" },
  { code: "GBCC", name: "Great Basin Coordination Center", location: "Salt Lake City, UT" },
  { code: "NRCC", name: "Northern Rockies Coordination Center", location: "Missoula, MT" },
  { code: "NWCC", name: "Northwest Interagency Coordination Center", location: "Portland, OR" },
  { code: "ONC", name: "Operations Northern California", location: "Redding, CA" },
  { code: "OSC", name: "Operations Southern California", location: "Riverside, CA" },
  { code: "RMCC", name: "Rocky Mountain Area Coordination Center", location: "Lakewood, CO" },
  { code: "SACC", name: "Southern Area Coordination Center", location: "Atlanta, GA" },
  { code: "SWCC", name: "Southwest Coordination Center", location: "Albuquerque, NM" },
];

window.gaccInfo = function (code) {
  return window.GACCS.find((g) => g.code === code) || { code, name: "", location: "" };
};
