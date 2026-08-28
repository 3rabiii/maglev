package restapi

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"maglev.onebusaway.org/gtfsdb"
	"maglev.onebusaway.org/internal/models"
	"maglev.onebusaway.org/internal/nulls"
	"maglev.onebusaway.org/internal/utils"
)

// stopArrivalsInput identifies one stop and the window to compute arrivals over.
// Location is the agency timezone the window is anchored in; QueryTime must
// already be expressed in it.
type stopArrivalsInput struct {
	StopCode   string
	AgencyID   string
	Location   *time.Location
	QueryTime  time.Time
	Before     time.Duration
	After      time.Duration
	RouteTypes []int // nil or empty means no route-type filter
}

// arrivalsAccumulator gathers the entities that the arrivals of one or more
// stops reference, so a caller looping over several stops builds a single
// deduplicated references block. Construct it with newArrivalsAccumulator.
type arrivalsAccumulator struct {
	trips      map[string]*gtfsdb.Trip
	routes     map[string]*gtfsdb.Route
	stopIDs    map[string]bool
	situations *situationCollector

	// alertAgencyID is the agency alerts are namespaced under. It starts as the
	// caller's primary agency and, only when that is empty, adopts the first
	// route agency seen.
	alertAgencyID string
}

func newArrivalsAccumulator(primaryAgencyID string) *arrivalsAccumulator {
	return &arrivalsAccumulator{
		trips:         make(map[string]*gtfsdb.Trip),
		routes:        make(map[string]*gtfsdb.Route),
		stopIDs:       make(map[string]bool),
		situations:    newSituationCollector(),
		alertAgencyID: primaryAgencyID,
	}
}

// stopArrivalsResult is what one stop contributed to a request.
type stopArrivalsResult struct {
	Arrivals []models.ArrivalAndDeparture

	// Matched reports whether any stop_time fell inside the window at all,
	// which is distinct from Arrivals being empty (every matched row can still
	// be dropped for a missing route or trip). The per-stop handler short-
	// circuits reference and nearby-stop work when nothing matched.
	Matched bool
}

// activeStopTime pairs a stop_time row with the service date it was matched on,
// since the ±1-day window can match the same trip on adjacent service days.
type activeStopTime struct {
	gtfsdb.GetStopTimesForStopInWindowRow
	ServiceDate time.Time
}

// arrivalsForStop computes the arrivals and departures for a single stop over
// the requested window, recording the routes, trips, stops and situations they
// reference into acc.
//
// Callers are responsible for installing a request-scoped snapshot cache
// (WithSnapshotCache) before the first call — BuildTripStatus is invoked once
// per arrival row, and across a wide window or many stops the uncached compute
// chain dominates the request.
func (api *RestAPI) arrivalsForStop(ctx context.Context, in stopArrivalsInput, acc *arrivalsAccumulator) (stopArrivalsResult, error) {
	stopID := utils.FormCombinedID(in.AgencyID, in.StopCode)
	result := stopArrivalsResult{Arrivals: make([]models.ArrivalAndDeparture, 0)}

	allActiveStopTimes, err := api.activeStopTimesForWindow(ctx, in)
	if err != nil {
		return result, err
	}
	if len(allActiveStopTimes) == 0 {
		return result, nil
	}
	result.Matched = true

	acc.stopIDs[in.StopCode] = true

	routesLookup, tripsLookup, tripStopCountMap, err := api.batchArrivalEntities(ctx, allActiveStopTimes)
	if err != nil {
		return result, err
	}

	for _, ast := range allActiveStopTimes {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}

		st := ast.GetStopTimesForStopInWindowRow
		serviceMidnight := ast.ServiceDate

		route, routeExists := routesLookup[st.RouteID]
		if !routeExists {
			api.Logger.Debug("skipping stop time: route not found in batch fetch",
				slog.String("routeID", st.RouteID),
				slog.String("tripID", st.TripID))
			continue
		}

		trip, tripExists := tripsLookup[st.TripID]
		if !tripExists {
			api.Logger.Debug("skipping stop time: trip not found in batch fetch",
				slog.String("tripID", st.TripID),
				slog.String("routeID", st.RouteID))
			continue
		}

		if !isRouteTypeAllowed(route.Type, in.RouteTypes) {
			continue
		}

		rCopy := route
		acc.routes[route.ID] = &rCopy
		tCopy := trip
		acc.trips[trip.ID] = &tCopy

		arrival := api.buildArrival(ctx, arrivalInput{
			stopTime:         st,
			route:            route,
			serviceMidnight:  serviceMidnight,
			queryTime:        in.QueryTime,
			stopCode:         in.StopCode,
			stopID:           stopID,
			totalStopsInTrip: tripStopCountMap[st.TripID],
		}, acc)

		result.Arrivals = append(result.Arrivals, *arrival)
	}

	return result, nil
}

// activeStopTimesForWindow collects the stop_times falling inside the request
// window across yesterday, today and tomorrow, so trips whose service day
// started before midnight are not dropped.
func (api *RestAPI) activeStopTimesForWindow(ctx context.Context, in stopArrivalsInput) ([]activeStopTime, error) {
	windowStart := in.QueryTime.Add(-in.Before)
	windowEnd := in.QueryTime.Add(in.After)

	var allActiveStopTimes []activeStopTime

	for dayOffset := -1; dayOffset <= 1; dayOffset++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		targetDate := in.QueryTime.AddDate(0, 0, dayOffset)
		serviceMidnight := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 0, 0, 0, 0, in.Location)
		serviceDateStr := targetDate.Format("20060102")

		activeServiceIDs, err := api.GtfsManager.GtfsDB.Queries.GetActiveServiceIDsForDate(ctx, serviceDateStr)
		if err != nil {
			// dayOffset==0 is the user's actual service date — silently
			// dropping it would emit a 200 with the most important day's
			// arrivals missing. Fail loud for that case so clients can
			// retry. ±1-day failures stay best-effort (window-spillover only).
			if dayOffset == 0 {
				return nil, fmt.Errorf("query active service IDs for %s: %w", serviceDateStr, err)
			}
			api.Logger.Warn("failed to query active service IDs for window-spillover day, skipping",
				slog.String("date", serviceDateStr),
				slog.Int("day_offset", dayOffset),
				slog.Any("error", err))
			continue
		}
		if len(activeServiceIDs) == 0 {
			continue
		}

		activeServiceIDSet := make(map[string]bool, len(activeServiceIDs))
		for _, sid := range activeServiceIDs {
			activeServiceIDSet[sid] = true
		}

		startOffset := windowStart.Sub(serviceMidnight)
		endOffset := windowEnd.Sub(serviceMidnight)
		if endOffset < 0 {
			continue
		}

		stopTimes, err := api.GtfsManager.GtfsDB.Queries.GetStopTimesForStopInWindow(ctx, gtfsdb.GetStopTimesForStopInWindowParams{
			StopID:           in.StopCode,
			WindowStartNanos: startOffset.Nanoseconds(),
			WindowEndNanos:   endOffset.Nanoseconds(),
		})
		if err != nil {
			api.Logger.Warn("failed to query stop times in window",
				slog.String("stopID", in.StopCode),
				slog.Any("error", err))
			continue
		}

		for _, st := range stopTimes {
			if activeServiceIDSet[st.ServiceID] {
				allActiveStopTimes = append(allActiveStopTimes, activeStopTime{
					GetStopTimesForStopInWindowRow: st,
					ServiceDate:                    serviceMidnight,
				})
			}
		}
	}

	return allActiveStopTimes, nil
}

// batchArrivalEntities resolves every route, trip and per-trip stop count the
// matched stop_times need in three queries rather than per row.
func (api *RestAPI) batchArrivalEntities(ctx context.Context, allActiveStopTimes []activeStopTime) (
	routesLookup map[string]gtfsdb.Route,
	tripsLookup map[string]gtfsdb.Trip,
	tripStopCountMap map[string]int,
	err error,
) {
	batchRouteIDs := make(map[string]bool)
	batchTripIDs := make(map[string]bool)

	for _, ast := range allActiveStopTimes {
		st := ast.GetStopTimesForStopInWindowRow
		if st.RouteID != "" {
			batchRouteIDs[st.RouteID] = true
		}
		if st.TripID != "" {
			batchTripIDs[st.TripID] = true
		}
	}

	uniqueRouteIDs := make([]string, 0, len(batchRouteIDs))
	for id := range batchRouteIDs {
		uniqueRouteIDs = append(uniqueRouteIDs, id)
	}

	uniqueTripIDs := make([]string, 0, len(batchTripIDs))
	for id := range batchTripIDs {
		uniqueTripIDs = append(uniqueTripIDs, id)
	}

	allRoutes, err := api.GtfsManager.GtfsDB.Queries.GetRoutesByIDs(ctx, uniqueRouteIDs)
	if err != nil {
		return nil, nil, nil, err
	}

	allTrips, err := api.GtfsManager.GtfsDB.Queries.GetTripsByIDs(ctx, uniqueTripIDs)
	if err != nil {
		return nil, nil, nil, err
	}

	routesLookup = make(map[string]gtfsdb.Route, len(allRoutes))
	for _, route := range allRoutes {
		routesLookup[route.ID] = route
	}

	tripsLookup = make(map[string]gtfsdb.Trip, len(allTrips))
	for _, trip := range allTrips {
		tripsLookup[trip.ID] = trip
	}

	// Batch-fetch stop counts per trip to avoid per-arrival N+1 queries for totalStopsInTrip.
	tripStopCountMap = make(map[string]int, len(uniqueTripIDs))
	if len(uniqueTripIDs) > 0 {
		allStopTimesForTrips, stopTimesErr := api.GtfsManager.GtfsDB.Queries.GetStopTimesForTripIDs(ctx, uniqueTripIDs)
		if stopTimesErr != nil {
			api.Logger.Warn("failed to batch fetch stop times for trips", slog.Any("error", stopTimesErr))
		} else {
			for _, st := range allStopTimesForTrips {
				tripStopCountMap[st.TripID]++
			}
		}
	}

	return routesLookup, tripsLookup, tripStopCountMap, nil
}

// arrivalInput carries the per-row values buildArrival needs. Grouped into a
// struct because several are same-typed strings and times that would be
// indistinguishable as positional arguments.
type arrivalInput struct {
	stopTime         gtfsdb.GetStopTimesForStopInWindowRow
	route            gtfsdb.Route
	serviceMidnight  time.Time
	queryTime        time.Time
	stopCode         string
	stopID           string
	totalStopsInTrip int
}

// buildArrival turns one matched stop_time into an ArrivalAndDeparture,
// resolving its real-time prediction and trip status along the way.
func (api *RestAPI) buildArrival(ctx context.Context, in arrivalInput, acc *arrivalsAccumulator) *models.ArrivalAndDeparture {
	st := in.stopTime
	route := in.route

	scheduledArrivalTime := in.serviceMidnight.Add(time.Duration(st.ArrivalTime))
	scheduledDepartureTime := in.serviceMidnight.Add(time.Duration(st.DepartureTime))

	var (
		predictedArrivalTime   = scheduledArrivalTime
		predictedDepartureTime = scheduledDepartureTime
		predicted              = false
		vehicleID              string
		tripStatus             *models.TripStatus
		distanceFromStop       = 0.0
		numberOfStopsAway      = 0
	)

	// Get vehicle if available. The response's top-level `vehicleId`
	// is the combined {agencyId}_{vehicleId} form per spec, matching
	// tripStatus.vehicleId (set by BuildTripStatus below). Internal
	// lookups (GetVehicleForTrip / GetVehicleByID) use the raw RT id
	// unchanged; the combined form is an output-only concern.
	vehicle := api.GtfsManager.GetVehicleForTrip(ctx, st.TripID)
	if vehicle != nil && vehicle.Trip != nil {
		if vehicle.ID != nil {
			vehicleID = utils.FormCombinedID(route.AgencyID, vehicle.ID.ID)
		} else {
			api.Logger.Warn("vehicle with nil ID descriptor found for trip", "tripID", st.TripID)
		}
	}

	predArr, predDep, isPredicted := api.getPredictedTimes(
		st.TripID,
		in.stopCode,
		int64(st.StopSequence),
		scheduledArrivalTime,
		scheduledDepartureTime,
	)

	if isPredicted {
		predicted = true
		predictedArrivalTime = predArr
		predictedDepartureTime = predDep
	}

	// Always built — Java attaches a BlockLocation (real-time or scheduled) to
	// every arrival, so tripStatus is always non-null. The vehicle is passed
	// through rather than left nil so BuildTripStatus does not repeat the
	// GetVehicleForTrip lookup already done above for every arrival row.
	status, statusExtras, statusErr := api.BuildTripStatus(ctx, route.AgencyID, st.TripID, vehicle, in.serviceMidnight, in.queryTime)
	if statusErr != nil {
		api.Logger.Warn("BuildTripStatus failed for arrival",
			"tripID", st.TripID, "error", statusErr)
	}
	if status != nil {
		tripStatus = status
		api.recordTripStatusReferences(ctx, status, st.TripID, acc)

		// Reuse the snapshot BuildTripStatus already computed for this trip.
		// BuildTripStatus applies the same schedule-deviation shift internally,
		// so recomputing here just to run metricsForStop was doubling every
		// per-arrival snapshot cost — a real problem on the plural handler
		// where minutesBefore/minutesAfter can be 24h in each direction.
		if statusExtras.snapshot != nil {
			if d, n, ok := statusExtras.snapshot.metricsForStop(st.TripID, int(st.StopSequence)); ok {
				distanceFromStop = d
				numberOfStopsAway = n
			}
		}
	}

	if !predicted {
		predictedArrivalTime = time.Time{}
		predictedDepartureTime = time.Time{}
	}

	// BuildTripStatus (via calculateBlockTripSequence) already computed
	// this and set it on the status; reuse rather than redoing the block
	// lookup for every arrival row.
	blockTripSequence := 0
	if tripStatus != nil {
		blockTripSequence = tripStatus.BlockTripSequence
	}

	lastUpdateTime := api.GtfsManager.GetVehicleLastUpdateTime(vehicle)

	// BuildTripStatus already resolved this trip's situations. Reuse those
	// references so each arrival does not repeat the alert lookup and its
	// situationIds are guaranteed to match references.situations.
	situationIDs := acc.situations.addRefs(statusExtras.situations)

	if acc.alertAgencyID == "" && route.AgencyID != "" {
		acc.alertAgencyID = route.AgencyID
	}

	return models.NewArrivalAndDeparture(
		utils.FormCombinedID(route.AgencyID, route.ID),  // routeID
		route.ShortName.String,                          // routeShortName
		route.LongName.String,                           // routeLongName
		utils.FormCombinedID(route.AgencyID, st.TripID), // tripID
		st.TripHeadsign.String,                          // tripHeadsign
		in.stopID,                                       // stopID
		vehicleID,                                       // vehicleID
		in.serviceMidnight,                              // serviceDate
		scheduledArrivalTime,                            // scheduledArrivalTime
		scheduledDepartureTime,                          // scheduledDepartureTime
		predictedArrivalTime,                            // predictedArrivalTime
		predictedDepartureTime,                          // predictedDepartureTime
		lastUpdateTime,                                  // lastUpdateTime
		predicted,                                       // predicted
		true,                                            // arrivalEnabled
		true,                                            // departureEnabled
		int(st.StopSequence)-1,                          // stopSequence (Zero-based index)
		in.totalStopsInTrip,                             // totalStopsInTrip
		numberOfStopsAway,                               // numberOfStopsAway
		blockTripSequence,                               // blockTripSequence
		distanceFromStop,                                // distanceFromStop
		"default",                                       // status
		"",                                              // occupancyStatus
		"",                                              // predicted occupancy
		"",                                              // historical occupancy
		tripStatus,                                      // tripStatus
		situationIDs,                                    // situationIDs
	)
}

// recordTripStatusReferences pulls the stops and the reassigned active trip that
// a trip status points at into the references accumulator, so every ID the
// arrival emits resolves in the response.
func (api *RestAPI) recordTripStatusReferences(ctx context.Context, status *models.TripStatus, scheduledTripID string, acc *arrivalsAccumulator) {
	if status.NextStop != "" {
		if _, nextStopID, err := utils.ExtractAgencyIDAndCodeID(status.NextStop); err == nil {
			acc.stopIDs[nextStopID] = true
		}
	}
	if status.ClosestStop != "" {
		if _, closestStopID, err := utils.ExtractAgencyIDAndCodeID(status.ClosestStop); err == nil {
			acc.stopIDs[closestStopID] = true
		}
	}

	if status.ActiveTripID == "" {
		return
	}
	_, activeTripID, err := utils.ExtractAgencyIDAndCodeID(status.ActiveTripID)
	if err != nil || activeTripID == scheduledTripID {
		return
	}
	if _, exists := acc.trips[activeTripID]; exists {
		return
	}

	activeTrip, err := api.GtfsManager.GtfsDB.Queries.GetTrip(ctx, activeTripID)
	if err != nil {
		api.Logger.Debug("skipping active trip reference: trip not found",
			slog.String("activeTripID", activeTripID),
			slog.String("scheduledTripID", scheduledTripID),
			slog.Any("error", err))
		return
	}
	acc.trips[activeTrip.ID] = &activeTrip

	activeRoute, err := api.GtfsManager.GtfsDB.Queries.GetRoute(ctx, activeTrip.RouteID)
	if err != nil {
		api.Logger.Warn("failed to fetch route for active trip reference",
			"tripID", activeTripID, "routeID", activeTrip.RouteID, "error", err)
		return
	}
	acc.routes[activeRoute.ID] = &activeRoute
}

// isRouteTypeAllowed reports whether a route survives the routeType filter. An
// empty filter accepts everything.
func isRouteTypeAllowed(routeType int64, allowed []int) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, t := range allowed {
		if int64(t) == routeType {
			return true
		}
	}
	return false
}

// arrivalsReferencesInput describes how to namespace the entities gathered in an
// arrivalsAccumulator. stopAgencies maps a bare stop ID to its owning agency;
// stops missing from it fall back to fallbackAgencyID.
type arrivalsReferencesInput struct {
	fallbackAgencyID string
	stopAgencies     map[string]string
	primaryAgency    *gtfsdb.Agency
}

// buildArrivalsReferences assembles the references block for a set of arrivals:
// their trips, the stops those arrivals and trip statuses point at, the routes
// serving them, and every route's agency.
func (api *RestAPI) buildArrivalsReferences(ctx context.Context, in arrivalsReferencesInput, acc *arrivalsAccumulator) (*models.ReferencesModel, error) {
	references := models.NewEmptyReferences()

	addedAgencyIDs := make(map[string]bool)
	if in.primaryAgency != nil {
		references.Agencies = append(references.Agencies, models.AgencyReferenceFromDatabase(in.primaryAgency))
		addedAgencyIDs[in.primaryAgency.ID] = true
	}

	api.appendTripReferences(ctx, references, acc)

	if err := api.appendStopReferences(ctx, references, in, acc); err != nil {
		return nil, err
	}

	api.appendRouteReferences(ctx, references, addedAgencyIDs, acc)

	return references, nil
}

func (api *RestAPI) appendTripReferences(ctx context.Context, references *models.ReferencesModel, acc *arrivalsAccumulator) {
	for _, trip := range acc.trips {
		// Get the route to determine the correct agency for trip/route IDs
		route, ok := acc.routes[trip.RouteID]
		if !ok {
			fetchedRoute, err := api.GtfsManager.GtfsDB.Queries.GetRoute(ctx, trip.RouteID)
			if err != nil {
				api.Logger.Warn("failed to fetch route for trip reference", "tripID", trip.ID, "routeID", trip.RouteID, "error", err)
				continue // Skip instead of falling back to the stop's agency
			}
			route = &fetchedRoute
			acc.routes[trip.RouteID] = route
		}
		routeAgencyID := route.AgencyID

		tripRef := models.NewTripReference(
			utils.FormCombinedID(routeAgencyID, trip.ID),        // Use route agency for trip ID
			utils.FormCombinedID(routeAgencyID, trip.RouteID),   // Use route agency for route ID
			utils.FormCombinedID(routeAgencyID, trip.ServiceID), // Use route agency for service ID
			trip.TripHeadsign.String,
			"",
			strconv.FormatInt(trip.DirectionID.Int64, 10),
			utils.FormCombinedID(routeAgencyID, trip.BlockID.String), // Use route agency for block ID
			utils.FormCombinedID(routeAgencyID, trip.ShapeID.String), // Use route agency for shape ID
		)
		references.Trips = append(references.Trips, *tripRef)
	}
}

func (api *RestAPI) appendStopReferences(ctx context.Context, references *models.ReferencesModel, in arrivalsReferencesInput, acc *arrivalsAccumulator) error {
	// Batch-fetch all stop references in one shot instead of one query per stop.
	stopIDsSlice := make([]string, 0, len(acc.stopIDs))
	for sid := range acc.stopIDs {
		stopIDsSlice = append(stopIDsSlice, sid)
	}

	batchStops, err := api.GtfsManager.GtfsDB.Queries.GetStopsByIDs(ctx, stopIDsSlice)
	if err != nil {
		api.Logger.Warn("failed to batch fetch stop references", slog.Any("error", err))
		batchStops = nil
	}

	batchRoutesForStops, err := api.GtfsManager.GtfsDB.Queries.GetRoutesForStops(ctx, stopIDsSlice)
	if err != nil {
		api.Logger.Warn("failed to batch fetch routes for stop references", slog.Any("error", err))
		batchRoutesForStops = nil
	}

	stopsMap := make(map[string]gtfsdb.Stop, len(batchStops))
	for _, s := range batchStops {
		stopsMap[s.ID] = s
	}

	routesByStop := make(map[string][]gtfsdb.GetRoutesForStopsRow)
	for _, row := range batchRoutesForStops {
		routesByStop[row.StopID] = append(routesByStop[row.StopID], row)
	}

	for stopID := range acc.stopIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		stopData, ok := stopsMap[stopID]
		if !ok {
			api.Logger.Debug("skipping stop reference: stop not found", slog.String("stopID", stopID))
			continue
		}

		routesForThisStop := routesByStop[stopID]
		combinedRouteIDs := make([]string, len(routesForThisStop))
		for i, route := range routesForThisStop {
			// Use route.AgencyID instead of the stop's agency
			combinedRouteIDs[i] = utils.FormCombinedID(route.AgencyID, route.ID)

			if _, exists := acc.routes[route.ID]; !exists {
				routeCopy := gtfsdb.Route{
					ID:        route.ID,
					AgencyID:  route.AgencyID,
					ShortName: route.ShortName,
					LongName:  route.LongName,
					Desc:      route.Desc,
					Type:      route.Type,
					Url:       route.Url,
					Color:     route.Color,
					TextColor: route.TextColor,
				}
				acc.routes[route.ID] = &routeCopy
			}
		}

		stopAgencyID := in.fallbackAgencyID
		if agencyID, ok := in.stopAgencies[stopID]; ok && agencyID != "" {
			stopAgencyID = agencyID
		}

		// NOTE: deliberately not buildStopModel — that helper defaults Code to
		// the stop ID when stops.code is NULL, which would change this
		// endpoint's existing output.
		references.Stops = append(references.Stops, models.Stop{
			ID:                 utils.FormCombinedID(stopAgencyID, stopData.ID),
			Name:               stopData.Name.String,
			Lat:                stopData.Lat,
			Lon:                stopData.Lon,
			Code:               stopData.Code.String,
			Direction:          api.DirectionCalculator.CalculateStopDirection(ctx, stopData.ID, stopData.Direction),
			LocationType:       int(stopData.LocationType.Int64),
			WheelchairBoarding: utils.MapWheelchairBoarding(nulls.WheelchairBoardingOrUnknown(stopData.WheelchairBoarding)),
			RouteIDs:           combinedRouteIDs,
			StaticRouteIDs:     combinedRouteIDs,
		})
	}

	return nil
}

func (api *RestAPI) appendRouteReferences(ctx context.Context, references *models.ReferencesModel, addedAgencyIDs map[string]bool, acc *arrivalsAccumulator) {
	for _, route := range acc.routes {
		references.Routes = append(references.Routes, models.NewRoute(
			utils.FormCombinedID(route.AgencyID, route.ID),
			route.AgencyID,
			route.ShortName.String,
			route.LongName.String,
			route.Desc.String,
			models.RouteType(route.Type),
			route.Url.String,
			route.Color.String,
			route.TextColor.String))

		// Add route agency to references if not already added
		if !addedAgencyIDs[route.AgencyID] {
			routeAgency, err := api.GtfsManager.GtfsDB.Queries.GetAgency(ctx, route.AgencyID)
			if err != nil {
				api.Logger.Warn("failed to fetch route agency for reference", "agencyID", route.AgencyID, "error", err)
				continue
			}
			references.Agencies = append(references.Agencies, models.AgencyReferenceFromDatabase(&routeAgency))
			addedAgencyIDs[route.AgencyID] = true
		}
	}
}
