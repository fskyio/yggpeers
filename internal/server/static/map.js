(function() {
	'use strict';

	var status = document.getElementById('map-status');
	var buttons = document.querySelectorAll('.toggle-btn');
	function showStatus(message) {
		status.textContent = message;
		status.hidden = false;
	}

	if (!window.maplibregl || !window.pmtiles) {
		showStatus('The map could not be loaded. Please reload to try again.');
		return;
	}
	function webglSupported() {
		try {
			var canvas = document.createElement('canvas');
			return !!(canvas.getContext('webgl2') || canvas.getContext('webgl'));
		} catch (error) {
			return false;
		}
	}
	if (!webglSupported()) {
		document.getElementById('webgl-warning').style.display = 'block';
		document.getElementById('legend').style.display = 'none';
		return;
	}

	var protocol = new pmtiles.Protocol();
	maplibregl.addProtocol('pmtiles', protocol.tile);
	var displayMode = 'total';
	var colors = ['#c2e699', '#78c679', '#31a354', '#006837', '#0c3a24'];
	function colorExpression(count) {
		return ['step', count, 'rgba(0,0,0,0)', 1, colors[0], 3, colors[1],
			5, colors[2], 10, colors[3], 20, colors[4]];
	}
	var map = new maplibregl.Map({
		container: 'map',
		style: {
			version: 8,
			sources: {
				countries: {
					type: 'vector',
					url: 'pmtiles://' + new URL('/static/countries.pmtiles', location.href).href,
					promoteId: 'code',
					attribution: '<a href="https://www.naturalearthdata.com/">Natural Earth</a>'
				},
				'small-places': {
					type: 'geojson',
					data: { type: 'FeatureCollection', features: [] }
				}
			},
			layers: [
				{ id: 'background', type: 'background', paint: { 'background-color': '#e8eef2' } },
				{
					id: 'countries-base', type: 'fill', source: 'countries', 'source-layer': 'countries',
					paint: { 'fill-color': '#fafaf8' }
				},
				{
					id: 'countries-fill', type: 'fill', source: 'countries', 'source-layer': 'countries',
					paint: {
						'fill-color': colorExpression(['coalesce', ['feature-state', displayMode], 0]),
						'fill-opacity': 0.7
					}
				},
				{
					id: 'countries-outline', type: 'line', source: 'countries', 'source-layer': 'countries',
					paint: { 'line-color': '#888', 'line-width': 0.5 }
				},
				{
					id: 'small-places', type: 'circle', source: 'small-places',
					paint: {
						'circle-radius': 5,
						'circle-color': colorExpression(['get', displayMode]),
						'circle-stroke-width': 1.5,
						'circle-stroke-color': '#fff'
					}
				}
			]
		},
		center: [0, 30],
		zoom: 2,
		maxZoom: 12,
		renderWorldCopies: false
	});
	map.on('error', function(e) {
		console.error('Map load failed:', e.error);
		showStatus('Some map boundaries could not be loaded. Please reload to try again.');
	});

	function fetchJSON(url) {
		return fetch(url).then(function(response) {
			if (!response.ok) throw new Error(url + ': HTTP ' + response.status);
			return response.json();
		});
	}
	function normalize(name) {
		return name.trim().toLowerCase();
	}

	map.on('load', function() {
		Promise.all([fetchJSON('/api/countries'), fetchJSON('/static/countries.json')]).then(function(results) {
			var counts = results[0];
			var countries = results[1];
			var aliases = Object.create(null);
			var countsByCode = Object.create(null);
			Object.keys(countries).forEach(function(code) {
				countries[code].aliases.forEach(function(name) { aliases[normalize(name)] = code; });
			});
			Object.keys(counts).forEach(function(name) {
				var code = aliases[normalize(name)];
				if (!code) {
					console.warn('No map boundary for peer country:', name);
					return;
				}
				var info = counts[name];
				var previous = countsByCode[code];
				if (previous) {
					var weight = previous.total + info.total;
					var uptime = previous.avgUptime === null || info.avgUptime === null ? null :
						(previous.avgUptime * previous.total + info.avgUptime * info.total) / weight;
					info = { total: weight, online: previous.online + info.online, avgUptime: uptime };
				}
				countsByCode[code] = info;
			});
			var markers = [];
			Object.keys(countries).forEach(function(code) {
				var country = countries[code];
				var info = countsByCode[code] || { total: 0, online: 0, avgUptime: null };
				// Feature state survives loading new tiles and is independent of the archive.
				map.setFeatureState({ source: 'countries', sourceLayer: 'countries', id: code }, info);
				if (country.marker && info.total > 0) {
					markers.push({
						type: 'Feature',
						geometry: { type: 'Point', coordinates: country.marker },
						properties: { code: code, markerZoom: country.markerZoom, total: info.total, online: info.online }
					});
				}
			});
			map.getSource('small-places').setData({ type: 'FeatureCollection', features: markers });

			var markerZoom;
			function updateMarkerFilter() {
				markerZoom = Math.floor(map.getZoom());
				map.setFilter('small-places', ['all', ['>', ['get', displayMode], 0],
					['>', ['get', 'markerZoom'], markerZoom]]);
			}
			updateMarkerFilter();
			map.on('zoom', function() {
				if (Math.floor(map.getZoom()) !== markerZoom) updateMarkerFilter();
			});

			var popup = new maplibregl.Popup({ closeButton: false, closeOnClick: false });
			function showCountry(feature, lngLat) {
				var code = feature.properties.code;
				var country = countries[code];
				var info = countsByCode[code] || { total: 0, online: 0, avgUptime: null };
				var content = document.createElement('div');
				var title = document.createElement('strong');
				title.textContent = country ? country.name : feature.properties.name;
				content.appendChild(title);
				function line(text) {
					content.appendChild(document.createElement('br'));
					content.appendChild(document.createTextNode(text));
				}
				if (info.total > 0) {
					line(info.total + ' total peer' + (info.total !== 1 ? 's' : ''));
					line(info.online + ' online, ' + Math.round(info.online / info.total * 100) + '% online');
					if (info.avgUptime !== null) line(Number(info.avgUptime).toFixed(1) + '% avg. uptime');
				} else {
					line('No peers');
				}
				popup.setLngLat(lngLat).setDOMContent(content).addTo(map);
			}
			// Query the topmost feature so small-place markers take precedence over land.
			map.on('mousemove', function(e) {
				var features = map.queryRenderedFeatures(e.point, { layers: ['small-places', 'countries-fill'] });
				map.getCanvas().style.cursor = features.length ? 'pointer' : '';
				if (features.length) showCountry(features[0], e.lngLat);
				else popup.remove();
			});
			map.getCanvas().addEventListener('mouseleave', function() { popup.remove(); });
			map.on('movestart', function() { popup.remove(); });
			map.on('click', function(e) {
				var features = map.queryRenderedFeatures(e.point, { layers: ['small-places', 'countries-fill'] });
				if (!features.length) return;
				var feature = features[0];
				if (feature.layer.id === 'small-places') {
					var country = countries[feature.properties.code];
					map.flyTo({ center: country.marker, zoom: Math.min(country.markerZoom + 3, 12) });
				} else {
					showCountry(feature, e.lngLat);
				}
			});

			buttons.forEach(function(button) {
				button.disabled = false;
				button.addEventListener('click', function() {
					displayMode = this.getAttribute('data-mode');
					buttons.forEach(function(b) {
						var active = b.getAttribute('data-mode') === displayMode;
						b.classList.toggle('active', active);
						b.setAttribute('aria-pressed', active);
					});
					map.setPaintProperty('countries-fill', 'fill-color', colorExpression(['coalesce', ['feature-state', displayMode], 0]));
					map.setPaintProperty('small-places', 'circle-color', colorExpression(['get', displayMode]));
					updateMarkerFilter();
				});
			});
		}).catch(function(error) {
			console.error('Peer map data failed:', error);
			showStatus('Peer counts could not be loaded. Please reload to try again.');
		});
	});
})();
