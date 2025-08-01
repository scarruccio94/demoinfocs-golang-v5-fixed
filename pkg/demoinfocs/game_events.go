package demoinfocs

import (
	"fmt"
	"math"
	"time"

	"github.com/golang/geo/r3"
	"github.com/markus-wa/go-unassert"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	common "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	events "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
	msg "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
)

func (p *parser) handleGameEventList(gel *msg.CMsgSource1LegacyGameEventList) {
	p.gameEventDescs = make(map[int32]*msg.CMsgSource1LegacyGameEventListDescriptorT)
	for _, d := range gel.GetDescriptors() {
		p.gameEventDescs[d.GetEventid()] = d
	}
}

func (p *parser) handleGameEvent(ge *msg.CMsgSource1LegacyGameEvent) {
	if p.gameEventDescs == nil {
		p.eventDispatcher.Dispatch(events.ParserWarn{
			Message: "received GameEvent but event descriptors are missing",
			Type:    events.WarnTypeGameEventBeforeDescriptors,
		})

		list := new(msg.CMsgSource1LegacyGameEventList)

		err := proto.Unmarshal(p.source2FallbackGameEventListBin, list)
		if err != nil {
			p.setError(err)

			return
		}

		p.handleGameEventList(list)
	}

	desc := p.gameEventDescs[ge.GetEventid()]

	debugGameEvent(desc, ge)

	data := mapGameEventData(desc, ge)

	if handler, eventKnown := p.gameEventHandler.gameEventNameToHandler[desc.GetName()]; eventKnown {
		// some events are known but have no handler
		// these will just be dispatched as GenericGameEvent
		if handler != nil {
			handler(data)
		}
	} else {
		p.eventDispatcher.Dispatch(events.ParserWarn{Message: fmt.Sprintf("unknown event %q", desc.GetName())})
		unassert.Error("unknown event %q", desc.GetName())
	}

	p.eventDispatcher.Dispatch(events.GenericGameEvent{
		Name: desc.GetName(),
		Data: data,
	})
}

type gameEventHandler struct {
	parser                      *parser
	gameEventNameToHandler      map[string]gameEventHandlerFunc
	userIDToFallDamageFrame     map[int32]int
	frameToRoundEndReason       map[int]events.RoundEndReason
	ignoreBombsiteIndexNotFound bool // see https://github.com/markus-wa/demoinfocs-golang/issues/314
}

func (geh gameEventHandler) dispatch(event any) {
	geh.parser.eventDispatcher.Dispatch(event)
}

func (geh gameEventHandler) gameState() *gameState {
	return geh.parser.gameState
}

func (geh gameEventHandler) playerByUserID(userID int) *common.Player {
	player := geh.gameState().playersByUserID[userID]
	if player != nil {
		return player
	}

	rawInfo := geh.parser.rawPlayers[userID]
	if rawInfo == nil {
		return nil
	}

	return geh.gameState().playersByUserID[rawInfo.UserID]
}

func (geh gameEventHandler) playerByUserID32(userID int32) *common.Player {
	if userID <= math.MaxUint16 {
		userID &= 0xff
	}

	return geh.playerByUserID(int(userID))
}

type gameEventHandlerFunc func(map[string]*msg.CMsgSource1LegacyGameEventKeyT)

//nolint:funlen
func newGameEventHandler(parser *parser, ignoreBombsiteIndexNotFound bool) gameEventHandler {
	geh := gameEventHandler{
		parser:                      parser,
		userIDToFallDamageFrame:     make(map[int32]int),
		frameToRoundEndReason:       make(map[int]events.RoundEndReason),
		ignoreBombsiteIndexNotFound: ignoreBombsiteIndexNotFound,
	}

	// some events need to be delayed until their data is available
	// some events can't be delayed because the required state is lost by the end of the tick
	// TODO: maybe we're supposed to delay all of them and store the data we need until the end of the tick
	delay := func(f gameEventHandlerFunc) gameEventHandlerFunc {
		return func(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
			parser.delayedEventHandlers = append(parser.delayedEventHandlers, func() {
				f(data)
			})
		}
	}

	// some events only need to be delayed at the start of the demo until players are connected
	delayIfNoPlayers := func(f gameEventHandlerFunc) gameEventHandlerFunc {
		return func(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
			if len(parser.gameState.playersByUserID) == 0 {
				delay(f)(data)
			} else {
				f(data)
			}
		}
	}

	geh.gameEventNameToHandler = map[string]gameEventHandlerFunc{
		// sorted alphabetically
		"ammo_pickup":                     nil,                                   // Dunno, only in locally recorded (POV) demo
		"announce_phase_end":              nil,                                   // Dunno
		"begin_new_match":                 geh.beginNewMatch,                     // Match started
		"bomb_beep":                       nil,                                   // Bomb beep
		"bomb_begindefuse":                delayIfNoPlayers(geh.bombBeginDefuse), // Defuse started
		"bomb_beginplant":                 delayIfNoPlayers(geh.bombBeginPlant),  // Plant started
		"bomb_defused":                    delayIfNoPlayers(geh.bombDefused),     // Defuse finished
		"bomb_dropped":                    delayIfNoPlayers(geh.bombDropped),     // Bomb dropped
		"bomb_exploded":                   delayIfNoPlayers(geh.bombExploded),    // Bomb exploded
		"bomb_pickup":                     delayIfNoPlayers(geh.bombPickup),      // Bomb picked up
		"bomb_planted":                    delayIfNoPlayers(geh.bombPlanted),     // Plant finished
		"bot_takeover":                    delay(geh.botTakeover),                // Bot got taken over
		"bullet_damage":                   delayIfNoPlayers(geh.bulletDamage),    // CS2 only
		"buytime_ended":                   nil,                                   // Not actually end of buy time, seems to only be sent once per game at the start
		"choppers_incoming_warning":       nil,                                   // Helicopters are coming (Danger zone mode)
		"cs_intermission":                 nil,                                   // Dunno, only in locally recorded (POV) demo
		"cs_match_end_restart":            nil,                                   // Yawn
		"cs_pre_restart":                  nil,                                   // Not sure, doesn't seem to be important
		"cs_round_final_beep":             nil,                                   // Final beep
		"cs_round_start_beep":             nil,                                   // Round start beeps
		"cs_win_panel_match":              geh.csWinPanelMatch,                   // Not sure, maybe match end event???
		"cs_win_panel_round":              nil,                                   // Win panel, (==end of match?)
		"decoy_detonate":                  geh.decoyDetonate,                     // Decoy exploded/expired
		"decoy_started":                   delay(geh.decoyStarted),               // Decoy started. Delayed because projectile entity is not yet created
		"endmatch_cmm_start_reveal_items": nil,                                   // Drops
		"entity_visible":                  nil,                                   // Dunno, only in locally recorded (POV) demo
		"enter_bombzone":                  nil,                                   // Dunno, only in locally recorded (POV) demo
		"exit_bombzone":                   nil,                                   // Dunno, only in locally recorded (POV) demo
		"enter_buyzone":                   nil,                                   // Dunno, only in locally recorded (POV) demo
		"exit_buyzone":                    nil,                                   // Dunno, only in locally recorded (POV) demo
		"flashbang_detonate":              geh.flashBangDetonate,                 // Flash exploded
		"firstbombs_incoming_warning":     nil,                                   // First wave artillery incoming (Danger zone mode)
		"grenade_thrown":                  nil,                                   // CS2 only, not reliable as it's not always present in demos and always fired. You should use "weapon_fire".
		"hegrenade_detonate":              geh.heGrenadeDetonate,                 // HE exploded
		"hostage_killed":                  geh.hostageKilled,                     // Hostage killed
		"hostage_hurt":                    geh.hostageHurt,                       // Hostage hurt
		"hostage_rescued":                 geh.hostageRescued,                    // Hostage rescued
		"hostage_rescued_all":             geh.HostageRescuedAll,                 // All hostages rescued
		"hltv_chase":                      nil,                                   // Don't care
		"hltv_fixed":                      nil,                                   // Dunno
		"hltv_message":                    nil,                                   // No clue
		"hltv_status":                     nil,                                   // Don't know
		"hltv_title":                      nil,                                   // Don't know
		"hostname_changed":                nil,                                   // Only present in locally recorded (POV) demos
		"inferno_expire":                  geh.infernoExpire,                     // Incendiary expired
		"inferno_startburn":               delay(geh.infernoStartBurn),           // Incendiary exploded/started. Delayed because inferno entity is not yet created
		"inspect_weapon":                  nil,                                   // Dunno, only in locally recorded (POV) demos
		"item_equip":                      delay(geh.itemEquip),                  // Equipped / weapon swap, I think. Delayed because of #142 - Bot entity possibly not yet created
		"item_pickup":                     delay(geh.itemPickup),                 // Picked up or bought? Delayed because of #119 - Equipment.UniqueID()
		"item_pickup_slerp":               nil,                                   // Not sure, only in locally recorded (POV) demos
		"item_remove":                     geh.itemRemove,                        // Dropped?
		"jointeam_failed":                 nil,                                   // Dunno, only in locally recorded (POV) demos
		"other_death":                     geh.otherDeath,                        // Other deaths, like chickens.
		"player_activate":                 nil,                                   // CS2 POV demos
		"player_blind":                    delay(geh.playerBlind),                // Player got blinded by a flash. Delayed because Player.FlashDuration hasn't been updated yet
		"player_changename":               nil,                                   // Name change
		"player_connect":                  geh.playerConnect,                     // Bot connected or player reconnected, players normally come in via string tables & data tables
		"player_connect_full":             nil,                                   // Connecting finished
		"player_death":                    delayIfNoPlayers(geh.playerDeath),     // Player died
		"player_disconnect":               geh.playerDisconnect,                  // Player disconnected (kicked, quit, timed out etc.)
		"player_falldamage":               geh.playerFallDamage,                  // Falldamage
		"player_footstep":                 delayIfNoPlayers(geh.playerFootstep),  // Footstep sound.- Delayed because otherwise Player might be nil
		"player_hurt":                     geh.playerHurt,                        // Player got hurt
		"player_jump":                     geh.playerJump,                        // Player jumped
		"player_spawn":                    nil,                                   // Player spawn
		"player_spawned":                  nil,                                   // Only present in locally recorded (POV) demos
		"player_given_c4":                 nil,                                   // Dunno, only present in locally recorded (POV) demos
		"player_ping":                     nil,                                   // When a player uses the "ping system" added with the operation Broken Fang, only present in locally recorded (POV) demos
		"player_ping_stop":                nil,                                   // When a player's ping expired, only present in locally recorded (POV) demos
		"player_sound":                    delayIfNoPlayers(geh.playerSound),     // When a player makes a sound

		// Player changed team. Delayed for two reasons
		// - team IDs of other players changing teams in the same tick might not have changed yet
		// - player entities might not have been re-created yet after a reconnect
		"player_team":                    delay(geh.playerTeam),
		"round_announce_final":           geh.roundAnnounceFinal,           // 30th round for normal de_, not necessarily matchpoint
		"round_announce_last_round_half": geh.roundAnnounceLastRoundHalf,   // Last round of the half
		"round_announce_match_point":     nil,                              // Match point announcement
		"round_announce_match_start":     geh.roundAnnounceMatchStart,      // Special match start announcement
		"round_announce_warmup":          nil,                              // Dunno
		"round_end":                      geh.roundEnd,                     // Round ended and the winner was announced
		"round_end_upload_stats":         nil,                              // Dunno, only present in POV demos
		"round_freeze_end":               geh.roundFreezeEnd,               // Round start freeze ended
		"round_mvp":                      geh.roundMVP,                     // Round MVP was announced
		"round_officially_ended":         geh.roundOfficiallyEnded,         // The event after which you get teleported to the spawn (=> You can still walk around between round_end and this event)
		"round_poststart":                nil,                              // Ditto
		"round_prestart":                 nil,                              // Ditto
		"round_start":                    geh.roundStart,                   // Round started
		"round_time_warning":             nil,                              // Round time warning
		"server_cvar":                    nil,                              // Dunno
		"show_survival_respawn_status":   nil,                              // Dunno, (Danger zone mode)
		"survival_paradrop_spawn":        nil,                              // A paradrop is coming (Danger zone mode)
		"smokegrenade_detonate":          geh.smokeGrenadeDetonate,         // Smoke popped
		"smokegrenade_expired":           geh.smokeGrenadeExpired,          // Smoke expired
		"switch_team":                    nil,                              // Dunno, only present in POV demos
		"tournament_reward":              nil,                              // Dunno
		"vote_cast":                      nil,                              // Dunno, only present in POV demos
		"weapon_fire":                    delayIfNoPlayers(geh.weaponFire), // Weapon was fired
		"weapon_fire_on_empty":           nil,                              // Sounds boring
		"weapon_reload":                  geh.weaponReload,                 // Weapon reloaded
		"weapon_zoom":                    nil,                              // Zooming in
		"weapon_zoom_rifle":              nil,                              // Dunno, only in locally recorded (POV) demo
		"entity_killed":                  nil,

		// S2
		"hltv_versioninfo": nil, // HLTV version info
	}

	return geh
}

func (geh gameEventHandler) clearGrenadeProjectiles() {
	// Issue #42
	// Sometimes grenades & infernos aren't deleted / destroyed via entity-updates at the end of the round,
	// so we need to do it here for those that weren't.
	//
	// We're not deleting them from entitites though as that's supposed to be as close to the actual demo data as possible.
	// We're also not using Entity.Destroy() because it would - in some cases - be called twice on the same entity
	// and it's supposed to be called when the demo actually says so (same case as with gameState.entities).
	for _, proj := range geh.gameState().grenadeProjectiles {
		geh.parser.nadeProjectileDestroyed(proj)
	}

	for _, inf := range geh.gameState().infernos {
		geh.parser.infernoExpired(inf)
	}

	// Thrown grenades could not be deleted at the end of the round (if they are thrown at the very end, they never get destroyed)
	geh.gameState().thrownGrenades = make(map[*common.Player]map[common.EquipmentType][]*common.Equipment)
	geh.gameState().flyingFlashbangs = make([]*FlyingFlashbang, 0)
}

func (geh gameEventHandler) roundStart(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	geh.clearGrenadeProjectiles()

	geh.dispatch(events.RoundStart{
		TimeLimit: int(data["timelimit"].GetValLong()),
		FragLimit: int(data["fraglimit"].GetValLong()),
		Objective: data["objective"].GetValString(),
	})
}

func (geh gameEventHandler) csWinPanelMatch(map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.AnnouncementWinPanelMatch{})
}

func (geh gameEventHandler) roundAnnounceFinal(map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.AnnouncementFinalRound{})
}

func (geh gameEventHandler) roundAnnounceMatchStart(map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.AnnouncementMatchStarted{})
}

func (geh gameEventHandler) roundAnnounceLastRoundHalf(map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.AnnouncementLastRoundHalf{})
}

func (geh gameEventHandler) roundEnd(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	winner := common.Team(data["winner"].GetValByte())
	winnerState := geh.gameState().Team(winner)

	var loserState *common.TeamState
	if winnerState != nil {
		loserState = winnerState.Opponent
	}

	reason := events.RoundEndReason(data["reason"].GetValByte())
	geh.frameToRoundEndReason[geh.parser.currentFrame] = reason

	geh.dispatch(events.RoundEnd{
		Message:     data["message"].GetValString(),
		Reason:      reason,
		Winner:      winner,
		WinnerState: winnerState,
		LoserState:  loserState,
	})
}

func (geh gameEventHandler) roundOfficiallyEnded(map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	geh.clearGrenadeProjectiles()

	geh.dispatch(events.RoundEndOfficial{})
}

func (geh gameEventHandler) roundMVP(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.RoundMVPAnnouncement{
		Player: geh.playerByUserID32(data["userid"].GetValShort()),
		Reason: events.RoundMVPReason(data["reason"].GetValShort()),
	})
}

func (geh gameEventHandler) botTakeover(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	taker := geh.playerByUserID32(data["userid"].GetValShort())

	unassert.True(!taker.IsBot)
	unassert.True(taker.IsControllingBot())
	unassert.NotNil(taker.ControlledBot())

	geh.dispatch(events.BotTakenOver{
		Taker: taker,
	})
}

func (geh gameEventHandler) beginNewMatch(map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.MatchStart{})
}

func (geh gameEventHandler) roundFreezeEnd(map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.RoundFreezetimeEnd{})
}

func (geh gameEventHandler) playerFootstep(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.Footstep{
		Player: geh.playerByUserID32(data["userid"].GetValShort()),
	})
}

func (geh gameEventHandler) playerJump(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.PlayerJump{
		Player: geh.playerByUserID32(data["userid"].GetValShort()),
	})
}

func (geh gameEventHandler) playerSound(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.PlayerSound{
		Player:   geh.playerByUserID32(data["userid"].GetValShort()),
		Radius:   int(data["radius"].GetValLong()),
		Duration: time.Duration(data["duration"].GetValFloat() * float32(time.Second)),
	})
}

func (geh gameEventHandler) weaponFire(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	shooter := geh.playerByUserID32(data["userid"].GetValShort())
	wepType := common.MapEquipment(data["weapon"].GetValString())

	geh.dispatch(events.WeaponFire{
		Shooter: shooter,
		Weapon:  getPlayerWeapon(shooter, wepType),
	})
}

func (geh gameEventHandler) weaponReload(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	pl := geh.playerByUserID32(data["userid"].GetValShort())
	if pl == nil {
		// see #162, "unknown" players since November 2019 update
		return
	}

	pl.IsReloading = true

	geh.dispatch(events.WeaponReload{
		Player: pl,
	})
}

func (geh gameEventHandler) playerDeath(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	killer := geh.playerByUserID32(data["attacker"].GetValShort())
	wepType := common.MapEquipment(data["weapon"].GetValString())
	victimUserID := data["userid"].GetValShort()
	wepType = geh.attackerWeaponType(wepType, victimUserID)
	if killer == nil && data["attacker_pawn"] != nil {
		// CS2 only, fallback to pawn handle if the killer was not found by its user ID
		killer = geh.parser.gameState.Participants().FindByPawnHandle(uint64(data["attacker_pawn"].GetValLong()))
	}

	geh.dispatch(events.Kill{
		Victim:            geh.playerByUserID32(data["userid"].GetValShort()),
		Killer:            killer,
		Assister:          geh.playerByUserID32(data["assister"].GetValShort()),
		IsHeadshot:        data["headshot"].GetValBool(),
		PenetratedObjects: int(data["penetrated"].GetValShort()),
		Weapon:            geh.getEquipmentInstance(killer, wepType),
		AssistedFlash:     data["assistedflash"].GetValBool(),
		AttackerBlind:     data["attackerblind"].GetValBool(),
		NoScope:           data["noscope"].GetValBool(),
		ThroughSmoke:      data["thrusmoke"].GetValBool(),
		Distance:          data["distance"].GetValFloat(),
	})
}

func (geh gameEventHandler) playerHurt(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	userID := data["userid"].GetValShort()
	player := geh.playerByUserID32(userID)
	attacker := geh.playerByUserID32(data["attacker"].GetValShort())

	wepType := common.MapEquipment(data["weapon"].GetValString())
	wepType = geh.attackerWeaponType(wepType, userID)

	health := int(data["health"].GetValByte())
	armor := int(data["armor"].GetValByte())

	healthDamage := int(data["dmg_health"].GetValShort())
	armorDamage := int(data["dmg_armor"].GetValByte())
	healthDamageTaken := healthDamage
	armorDamageTaken := armorDamage

	if healthDamageTaken > 100 {
		healthDamageTaken = 100
	}

	if armorDamageTaken > 100 {
		armorDamageTaken = 100
	}

	if player != nil {
		if health == 0 {
			healthDamageTaken = player.Health()
		}

		if armor == 0 {
			armorDamageTaken = player.Armor()
		}
	}

	geh.dispatch(events.PlayerHurt{
		Player:            player,
		Attacker:          attacker,
		Health:            health,
		Armor:             armor,
		HealthDamage:      healthDamage,
		ArmorDamage:       armorDamage,
		HealthDamageTaken: healthDamageTaken,
		ArmorDamageTaken:  armorDamageTaken,
		HitGroup:          events.HitGroup(data["hitgroup"].GetValByte()),
		Weapon:            geh.getEquipmentInstance(attacker, wepType),
	})
}

func (geh gameEventHandler) playerFallDamage(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.userIDToFallDamageFrame[data["userid"].GetValShort()] = geh.parser.currentFrame
}

func (geh gameEventHandler) playerBlind(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	attacker := geh.gameState().lastFlash.player
	projectile := geh.gameState().lastFlash.projectileByPlayer[attacker]
	unassert.NotNilf(projectile, "PlayerFlashed.Projectile should never be nil")

	if projectile != nil {
		unassert.Samef(attacker, projectile.Thrower, "PlayerFlashed.Attacker != PlayerFlashed.Projectile.Thrower")
		unassert.NotNilf(projectile.WeaponInstance, "WeaponInstance == nil")

		if projectile.WeaponInstance != nil {
			unassert.Samef(projectile.WeaponInstance.Type, common.EqFlash, "PlayerFlashed.Projectile.Weapon != EqFlash")
		}
	}

	geh.dispatch(events.PlayerFlashed{
		Player:     geh.playerByUserID32(data["userid"].GetValShort()),
		Attacker:   attacker,
		Projectile: projectile,
	})
}

func (geh gameEventHandler) flashBangDetonate(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {

	nadeEvent := geh.nadeEvent(data, common.EqFlash)

	geh.gameState().lastFlash.player = nadeEvent.Thrower

	if !geh.parser.disableMimicSource1GameEvents {
		geh.dispatch(events.FlashExplode{
			GrenadeEvent: nadeEvent,
		})
	}
}

func (geh gameEventHandler) heGrenadeDetonate(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.HeExplode{
		GrenadeEvent: geh.nadeEvent(data, common.EqHE),
	})
}

func (geh gameEventHandler) decoyStarted(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.DecoyStart{
		GrenadeEvent: geh.nadeEvent(data, common.EqDecoy),
	})
}

func (geh gameEventHandler) decoyDetonate(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	event := geh.nadeEvent(data, common.EqDecoy)
	geh.dispatch(events.DecoyExpired{
		GrenadeEvent: event,
	})

	geh.parser.delayedEventHandlers = append(geh.parser.delayedEventHandlers, func() {
		geh.deleteThrownGrenade(event.Thrower, common.EqDecoy)
	})
}

func (geh gameEventHandler) smokeGrenadeDetonate(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.SmokeStart{
		GrenadeEvent: geh.nadeEvent(data, common.EqSmoke),
	})
}

func (geh gameEventHandler) smokeGrenadeExpired(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	event := geh.nadeEvent(data, common.EqSmoke)
	geh.dispatch(events.SmokeExpired{
		GrenadeEvent: event,
	})

	geh.deleteThrownGrenade(event.Thrower, common.EqSmoke)
}

func (geh gameEventHandler) infernoStartBurn(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.FireGrenadeStart{
		GrenadeEvent: geh.nadeEvent(data, common.EqIncendiary),
	})
}

func (geh gameEventHandler) infernoExpire(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.FireGrenadeExpired{
		GrenadeEvent: geh.nadeEvent(data, common.EqIncendiary),
	})
}

func (geh gameEventHandler) hostageHurt(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	event := events.HostageHurt{
		Player:  geh.playerByUserID32(data["userid"].GetValShort()),
		Hostage: geh.gameState().hostages[int(data["hostage"].GetValShort())],
	}

	geh.dispatch(event)
}

func (geh gameEventHandler) hostageKilled(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	event := events.HostageKilled{
		Killer:  geh.playerByUserID32(data["userid"].GetValShort()),
		Hostage: geh.gameState().hostages[int(data["hostage"].GetValShort())],
	}

	geh.dispatch(event)
}

func (geh gameEventHandler) hostageRescued(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	event := events.HostageRescued{
		Player:  geh.playerByUserID32(data["userid"].GetValShort()),
		Hostage: geh.gameState().hostages[int(data["hostage"].GetValShort())],
	}

	geh.dispatch(event)
}

func (geh gameEventHandler) HostageRescuedAll(map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	geh.dispatch(events.HostageRescuedAll{})
}

func (geh gameEventHandler) bulletDamage(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	event := events.BulletDamage{
		Attacker:        geh.playerByUserID32(data["attacker"].GetValShort()),
		Victim:          geh.playerByUserID32(data["victim"].GetValShort()),
		Distance:        data["distance"].GetValFloat(),
		DamageDirX:      data["damage_dir_x"].GetValFloat(),
		DamageDirY:      data["damage_dir_y"].GetValFloat(),
		DamageDirZ:      data["damage_dir_z"].GetValFloat(),
		NumPenetrations: int(data["num_penetrations"].GetValShort()),
		IsNoScope:       data["no_scope"].GetValBool(),
		IsAttackerInAir: data["in_air"].GetValBool(),
	}

	geh.dispatch(event)
}

func (geh gameEventHandler) playerConnect(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	pl := common.PlayerInfo{
		UserID:       int(data["userid"].GetValShort()),
		Name:         data["name"].GetValString(),
		GUID:         data["networkid"].GetValString(),
		XUID:         data["xuid"].GetValUint64(),
		IsFakePlayer: data["bot"].GetValBool(),
	}

	if pl.GUID != "" && pl.XUID == 0 {
		var err error
		pl.XUID, err = guidToSteamID64(pl.GUID)

		if err != nil {
			geh.parser.setError(fmt.Errorf("failed to parse player XUID: %v", err.Error()))
		}
	}

	if !pl.IsFakePlayer && !pl.IsHltv && pl.XUID > 0 && pl.UserID <= math.MaxUint8 {
		pl.UserID |= math.MaxUint8 << 8
	}

	geh.parser.setRawPlayer(pl.UserID, pl)
}

func (geh gameEventHandler) playerDisconnect(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	uid := int(data["userid"].GetValShort())
	if uid <= math.MaxUint16 {
		uid &= 0xff
	}

	pl := geh.playerByUserID(uid)

	if pl != nil && pl.IsBot {
		geh.dispatch(events.PlayerDisconnected{
			Player: pl,
		})

		pl.IsConnected = false
	}
}

func (geh gameEventHandler) playerTeam(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	player := geh.playerByUserID32(data["userid"].GetValShort())
	newTeam := common.Team(data["team"].GetValByte())

	if player != nil {
		if player.Team != newTeam {
			// The "team" field may be incorrect with CS2 demos.
			// As the prop m_iTeamNum (bound to player.Team) is updated before the game-event is fired we can force
			// the correct team here.
			// https://github.com/markus-wa/demoinfocs-golang/issues/494
			newTeam = player.Team
			player.Team = newTeam
		}

		oldTeam := common.Team(data["oldteam"].GetValByte())

		geh.dispatch(events.PlayerTeamChange{
			Player:       player,
			IsBot:        data["isbot"].GetValBool(),
			Silent:       data["silent"].GetValBool(),
			NewTeam:      newTeam,
			NewTeamState: geh.gameState().Team(newTeam),
			OldTeam:      oldTeam,
			OldTeamState: geh.gameState().Team(oldTeam),
		})
	} else {
		// TODO: figure out why this happens and whether it's a bug or not
		geh.dispatch(events.ParserWarn{
			Message: "Player team swap game-event occurred but player is nil",
			Type:    events.WarnTypeTeamSwapPlayerNil,
		})
	}
}

func (geh gameEventHandler) bombBeginPlant(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	bombEvent, err := geh.bombEvent(data)
	if err != nil {
		geh.parser.setError(err)
		return
	}

	event := events.BombPlantBegin{BombEvent: bombEvent}
	event.Player.IsPlanting = true
	geh.parser.gameState.currentPlanter = event.Player
	geh.dispatch(event)
}

func (geh gameEventHandler) bombPlanted(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	bombEvent, err := geh.bombEvent(data)
	if err != nil {
		geh.parser.setError(err)
		return
	}

	event := events.BombPlanted{BombEvent: bombEvent}

	if event.Player != nil { // if not nil check is necessary for POV demos
		event.Player.IsPlanting = false
	}

	geh.parser.gameState.currentPlanter = nil
	geh.dispatch(event)
}

func (geh gameEventHandler) bombDefused(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	bombEvent, err := geh.bombEvent(data)
	if err != nil {
		geh.parser.setError(err)
		return
	}

	geh.gameState().currentDefuser = nil
	geh.dispatch(events.BombDefused{BombEvent: bombEvent})
}

func (geh gameEventHandler) bombExploded(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	bombEvent, err := geh.bombEvent(data)
	if err != nil {
		geh.parser.setError(err)
		return
	}

	geh.gameState().currentDefuser = nil
	geh.dispatch(events.BombExplode{BombEvent: bombEvent})
}

// ErrBombsiteIndexNotFound indicates that a game-event occurred that contained an unknown bombsite index.
// This error can be disabled by setting ParserConfig.IgnoreErrBombsiteIndexNotFound = true.
// See https://github.com/markus-wa/demoinfocs-golang/issues/314
var ErrBombsiteIndexNotFound = errors.New("bombsite index not found - see https://github.com/markus-wa/demoinfocs-golang/issues/314")

func (geh gameEventHandler) bombEvent(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) (events.BombEvent, error) {
	bombEvent := events.BombEvent{Player: geh.playerByUserID32(data["userid"].GetValShort())}

	const gameEventKeyTypeLong = 3

	var site int
	if data["site"].GetType() == gameEventKeyTypeLong {
		site = int(data["site"].GetValLong())
	} else {
		site = int(data["site"].GetValShort())
	}

	switch site {
	case geh.parser.bombsiteA.index:
		bombEvent.Site = events.BombsiteA
	case geh.parser.bombsiteB.index:
		bombEvent.Site = events.BombsiteB
	default:
		t := geh.parser.triggers[site]

		// when not found, only error if site is not 0, for retake games it may be 0 => unknown
		if t == nil {
			if !geh.ignoreBombsiteIndexNotFound {
				return bombEvent, errors.Wrapf(ErrBombsiteIndexNotFound, "bombsite with index %d not found", site)
			}
		} else {
			if t.contains(geh.parser.bombsiteA.center) {
				bombEvent.Site = events.BombsiteA
				geh.parser.bombsiteA.index = site
			} else if t.contains(geh.parser.bombsiteB.center) {
				bombEvent.Site = events.BombsiteB
				geh.parser.bombsiteB.index = site
			}
		}

		if bombEvent.Site == events.BomsiteUnknown {
			// this may occur on de_grind for bombsite B, really makes you think
			// see https://github.com/markus-wa/demoinfocs-golang/issues/280
			geh.dispatch(events.ParserWarn{
				Message: "bombsite unknown for bomb related event",
				Type:    events.WarnTypeBombsiteUnknown,
			})
		}
	}

	return bombEvent, nil
}

func (geh gameEventHandler) bombBeginDefuse(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	geh.gameState().currentDefuser = geh.playerByUserID32(data["userid"].GetValShort())

	geh.dispatch(events.BombDefuseStart{
		Player: geh.gameState().currentDefuser,
		HasKit: data["haskit"].GetValBool(),
	})
}

func (geh gameEventHandler) itemEquip(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	player, weapon := geh.itemEvent(data)
	geh.dispatch(events.ItemEquip{
		Player: player,
		Weapon: weapon,
	})
}

func (geh gameEventHandler) itemPickup(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	player, weapon := geh.itemEvent(data)
	geh.dispatch(events.ItemPickup{
		Player: player,
		Weapon: weapon,
	})
}

func (geh gameEventHandler) itemRemove(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	player, weapon := geh.itemEvent(data)
	geh.dispatch(events.ItemDrop{
		Player: player,
		Weapon: weapon,
	})
}

func (geh gameEventHandler) otherDeath(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	killer := geh.playerByUserID32(data["attacker"].GetValShort())
	otherType := data["othertype"].GetValString()
	otherID := data["otherid"].GetValShort()
	other := geh.gameState().entities[int(otherID)]

	var otherPosition r3.Vector

	if other != nil {
		otherPosition = other.Position()
	}

	wepType := common.MapEquipment(data["weapon"].GetValString())
	weapon := getPlayerWeapon(killer, wepType)

	geh.dispatch(events.OtherDeath{
		Killer:            killer,
		Weapon:            weapon,
		PenetratedObjects: int(data["penetrated"].GetValShort()),
		NoScope:           data["noscope"].GetValBool(),
		ThroughSmoke:      data["thrusmoke"].GetValBool(),
		KillerBlind:       data["attackerblind"].GetValBool(),

		OtherType:     otherType,
		OtherID:       otherID,
		OtherPosition: otherPosition,
	})
}

func (geh gameEventHandler) itemEvent(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) (*common.Player, *common.Equipment) {
	player := geh.playerByUserID32(data["userid"].GetValShort())

	wepType := common.MapEquipment(data["item"].GetValString())
	weapon := getPlayerWeapon(player, wepType)

	return player, weapon
}

func (geh gameEventHandler) bombDropped(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	player := geh.playerByUserID32(data["userid"].GetValShort())
	entityID := int(data["entityid"].GetValShort())

	geh.dispatch(events.BombDropped{
		Player:   player,
		EntityID: entityID,
	})
}

func (geh gameEventHandler) bombPickup(data map[string]*msg.CMsgSource1LegacyGameEventKeyT) {
	if !geh.parser.disableMimicSource1GameEvents {
		return
	}

	geh.dispatch(events.BombPickup{
		Player: geh.playerByUserID32(data["userid"].GetValShort()),
	})
}

// Just so we can nicely create GrenadeEvents in one line
func (geh gameEventHandler) nadeEvent(data map[string]*msg.CMsgSource1LegacyGameEventKeyT, nadeType common.EquipmentType) events.GrenadeEvent {
	var thrower *common.Player
	// Sometimes only the position and the entityid are present.
	// Since GetValShort() returns 0 for nil values, the thrower would be the player with UserID 0, so we need to check for the existence of the key.
	if data["userid"] != nil {
		thrower = geh.playerByUserID32(data["userid"].GetValShort())
	}

	// CS2 only - userid may be missing, but userid_pawn present.
	if thrower == nil && data["userid_pawn"] != nil {
		thrower = geh.gameState().Participants().FindByPawnHandle(uint64(data["userid_pawn"].GetValLong()))
	}

	position := r3.Vector{
		X: float64(data["x"].GetValFloat()),
		Y: float64(data["y"].GetValFloat()),
		Z: float64(data["z"].GetValFloat()),
	}
	nadeEntityID := int(data["entityid"].GetValShort())

	return events.GrenadeEvent{
		GrenadeType:     nadeType,
		Grenade:         geh.getThrownGrenade(thrower, nadeType),
		Thrower:         thrower,
		Position:        position,
		GrenadeEntityID: nadeEntityID,
	}
}

func (geh gameEventHandler) addThrownGrenade(p *common.Player, wep *common.Equipment) {
	if p == nil {
		// can happen for "unknown" players (see #162)
		return
	}

	gameState := geh.gameState()

	if gameState.thrownGrenades[p] == nil {
		gameState.thrownGrenades[p] = make(map[common.EquipmentType][]*common.Equipment)
	}

	gameState.thrownGrenades[p][wep.Type] = append(gameState.thrownGrenades[p][wep.Type], wep)
}

func (geh gameEventHandler) getThrownGrenade(p *common.Player, wepType common.EquipmentType) *common.Equipment {
	if p == nil {
		// can happen for incendiaries or "unknown" players (see #162)
		return nil
	}

	playerGrenades := geh.gameState().thrownGrenades[p]
	grenades := playerGrenades[wepType]

	if len(grenades) == 0 {
		// Molotovs/incendiaries may be reported as the opposite type in game-events. (i.e. incendiary reported as molotov and vice versa)
		switch wepType { //nolint:exhaustive
		case common.EqIncendiary:
			grenades = playerGrenades[common.EqMolotov]
		case common.EqMolotov:
			grenades = playerGrenades[common.EqIncendiary]
		}
	}

	if len(grenades) == 0 {
		// The player might be controlling a bot, in such case the thrown grenade is stored in the bot's state.
		bot := p.ControlledBot()
		if bot != nil && bot.SteamID64 != p.SteamID64 {
			return geh.getThrownGrenade(bot, wepType)
		}
	}

	if len(grenades) == 0 {
		return nil
	}

	return grenades[len(grenades)-1]
}

func (geh gameEventHandler) deleteThrownGrenade(p *common.Player, wepType common.EquipmentType) {
	if p == nil {
		// can happen for incendiaries or "unknown" players (see #162)
		return
	}

	playerGrenades := geh.gameState().thrownGrenades[p]
	if len(playerGrenades) == 0 {
		return
	}

	grenades := playerGrenades[wepType]
	if len(grenades) == 0 {
		return
	}

	// Delete the first grenade thrown by the player and this grenade type.
	playerGrenades[wepType] = grenades[:len(grenades)-1]
	if len(playerGrenades[wepType]) == 0 {
		delete(playerGrenades, wepType)
	}
}

func (geh gameEventHandler) attackerWeaponType(wepType common.EquipmentType, victimUserID int32) common.EquipmentType {
	// if the player took falldamage in this frame we set the weapon type to world damage
	if wepType == common.EqUnknown && geh.userIDToFallDamageFrame[victimUserID] == geh.parser.currentFrame {
		wepType = common.EqWorld
	}

	// if the round ended in the current frame with reason 1 or 0 we assume it was bomb damage
	// unfortunately RoundEndReasonTargetBombed isn't enough and sometimes we need to check for 0 as well
	if wepType == common.EqUnknown {
		switch geh.frameToRoundEndReason[geh.parser.currentFrame] {
		case 0:
			fallthrough
		case events.RoundEndReasonTargetBombed:
			wepType = common.EqBomb
		}
	}

	unassert.NotSame(wepType, common.EqUnknown)

	return wepType
}

func (geh gameEventHandler) getEquipmentInstance(player *common.Player, wepType common.EquipmentType) *common.Equipment {
	isGrenade := wepType.Class() == common.EqClassGrenade
	if isGrenade {
		return geh.getThrownGrenade(player, wepType)
	}

	return getPlayerWeapon(player, wepType)
}

// Returns the players instance of the weapon if applicable or a new instance otherwise.
func getPlayerWeapon(player *common.Player, wepType common.EquipmentType) *common.Equipment {
	if player != nil {
		alternateWepType := common.EquipmentAlternative(wepType)
		for _, wep := range player.Weapons() {
			if wep.Type == wepType || (alternateWepType != common.EqUnknown && wep.Type == alternateWepType) {
				return wep
			}
		}
	}

	wep := common.NewEquipment(wepType)

	return wep
}

func mapGameEventData(d *msg.CMsgSource1LegacyGameEventListDescriptorT, e *msg.CMsgSource1LegacyGameEvent) map[string]*msg.CMsgSource1LegacyGameEventKeyT {
	data := make(map[string]*msg.CMsgSource1LegacyGameEventKeyT, len(d.Keys))
	for i, k := range d.Keys {
		data[k.GetName()] = e.Keys[i]
	}

	return data
}

func guidToSteamID64(guid string) (uint64, error) {
	if guid == "BOT" {
		return 0, nil
	}

	steamID32, err := common.ConvertSteamIDTxtTo32(guid)
	if err != nil {
		return 0, err
	}

	return common.ConvertSteamID32To64(steamID32), nil
}

func (p *parser) dispatchMatchStartedEventIfNecessary() {
	if p.gameState.lastMatchStartedChangedEvent != nil {
		p.gameState.isMatchStarted = p.gameState.lastMatchStartedChangedEvent.NewIsStarted
		p.gameEventHandler.dispatch(*p.gameState.lastMatchStartedChangedEvent)
		p.gameState.lastMatchStartedChangedEvent = nil
	}
}

// Dispatch round progress events in the following order:
// 1. MatchStartedChanged
// 2. RoundStart
// 3. FreezeTimeStart
// 4. FreezetimeEnd
// 5. RoundEnd
// 6. MatchStartedChanged
func (p *parser) processRoundProgressEvents() {
	if p.gameState.lastRoundStartEvent != nil {
		p.dispatchMatchStartedEventIfNecessary()
		p.gameEventHandler.dispatch(*p.gameState.lastRoundStartEvent)
		p.gameState.lastRoundStartEvent = nil
	}

	if p.gameState.lastFreezeTimeChangedEvent != nil {
		p.gameEventHandler.dispatch(*p.gameState.lastFreezeTimeChangedEvent)
		p.gameState.lastFreezeTimeChangedEvent = nil
	}

	if p.gameState.lastRoundEndEvent != nil {
		p.gameEventHandler.dispatch(*p.gameState.lastRoundEndEvent)
		p.gameState.lastRoundEndEvent = nil
	}

	p.dispatchMatchStartedEventIfNecessary()
}

func (p *parser) processFlyingFlashbangs() {
	if len(p.gameState.flyingFlashbangs) == 0 {
		return
	}

	flashbang := p.gameState.flyingFlashbangs[0]
	if len(flashbang.flashedEntityIDs) == 0 {
		// Flashbang exploded and didn't flash any players, remove it from the queue
		if flashbang.explodedFrame > 0 && flashbang.explodedFrame < p.currentFrame {
			p.gameState.flyingFlashbangs = p.gameState.flyingFlashbangs[1:]
		}
		return
	}

	for _, entityID := range flashbang.flashedEntityIDs {
		player := p.gameState.Participants().ByEntityID()[entityID]
		if player == nil {
			continue
		}

		p.gameEventHandler.dispatch(events.PlayerFlashed{
			Player:     player,
			Attacker:   flashbang.projectile.Thrower,
			Projectile: flashbang.projectile,
		})
	}

	p.gameState.flyingFlashbangs = p.gameState.flyingFlashbangs[1:]
}

// Do some processing to dispatch game events at the end of the frame in correct order.
// This is necessary because some prop updates are not in a order that we would expect, e.g.:
// - The player prop m_flFlashDuration is updated after the game event player_blind have been parsed (used for CS:GO only)
// - The player prop m_flFlashDuration may be updated after *or* before the flashbang explosion event
// - Bomb props used to detect bomb events are updated after the prop m_eRoundWinReason used to detect round end events
//
// This makes sure game events are dispatched in a more expected order.
func (p *parser) processFrameGameEvents() {
	if !p.disableMimicSource1GameEvents {
		p.processFlyingFlashbangs()
		p.processRoundProgressEvents()
	}

	for _, eventHandler := range p.delayedEventHandlers {
		eventHandler()
	}

	p.delayedEventHandlers = p.delayedEventHandlers[:0]
}
