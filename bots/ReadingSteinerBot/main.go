// Bot ReadingSteinerBot: TAG + Monte Carlo para PokerFight.
//
// Estrategia:
//   - Preflop: selección tight-aggressive basada en Chen, ajustada por posición,
//     costo relativo al stack y presión de apuestas.
//   - Postflop: equity por Monte Carlo comparada con pot odds, con tamaños de
//     apuesta proporcionales al pozo y control de riesgo de stack.
//   - Seguridad: respeta acciones válidas, usa constructores del SDK (monto total
//     de ronda), y aplica fallback seguro ante cualquier situación rara.
package main

import (
	"context"
	"flag"
	"log"
	"math/rand/v2"
	"os"
	"time"

	"github.com/aicspucp/pokerfight-arena-participantes/pkg/botsdk"
	"github.com/aicspucp/pokerfight-arena-participantes/pkg/poker"
)

var (
	semilla      = flag.Uint64("semilla", 1, "semilla del generador aleatorio")
	simulaciones = flag.Int("simulaciones", 1200, "máximo de simulaciones Monte Carlo por decisión")
	detalle      = flag.Bool("detalle", false, "imprime decisiones en stderr")
)

const (
	fraccionDelPlazo    = 4
	presupuestoMaximo   = 900 * time.Millisecond
	presupuestoSinPlazo = 250 * time.Millisecond
	cadaCuantasReloj    = 128
)

type botReadingSteiner struct {
	azar     *rand.Rand
	maxSims  int
	registro *log.Logger
}

func (b *botReadingSteiner) Iniciar() error {
	b.azar = rand.New(rand.NewPCG(*semilla, *semilla^0xA24BAED4963EE407))
	b.maxSims = max(200, *simulaciones)
	if *detalle {
		b.registro = log.New(os.Stderr, "ReadingSteinerBot: ", log.Lmsgprefix)
	}
	return nil
}

func (*botReadingSteiner) Nombre() string { return "ReadingSteinerBot" }

func (b *botReadingSteiner) Decidir(ctx context.Context, t botsdk.Turno) botsdk.Accion {
	if !manoValida(t.Mano) {
		return accionSegura(t)
	}

	rivales := contarRivales(t)
	if rivales <= 0 {
		return accionSegura(t)
	}

	// Preflop: usar política tight-aggressive con awareness de posición/costo.
	if len(t.Estado.Comunitarias) == 0 {
		a := b.decidirPreflop(t, rivales)
		b.logDecision(t, -1, rivales, 0, a)
		return a
	}

	pres := presupuesto(t.Timeout)
	eq := estimarEquidad(ctx, b.azar, simulacion{
		mano:            t.Mano,
		comunitarias:    t.Estado.Comunitarias,
		rivales:         rivales,
		maxSimulaciones: b.maxSims,
		presupuesto:     pres,
	})

	a := b.decidirPostflop(t, eq.Valor, rivales)
	b.logDecision(t, eq.Valor, rivales, eq.Simulaciones, a)
	return a
}

func (b *botReadingSteiner) logDecision(t botsdk.Turno, eq float64, rivales, sims int, a botsdk.Accion) {
	if b.registro == nil {
		return
	}
	if eq < 0 {
		b.registro.Printf("%s %s%s riv=%d costo=%d pozo=%d -> %s %d",
			t.Estado.Etapa, t.Mano[0], t.Mano[1], rivales, t.PorIgualar(), t.Estado.Pozo, a.Tipo, a.Monto)
		return
	}
	b.registro.Printf("%s %s%s eq=%.3f riv=%d sims=%d costo=%d pozo=%d -> %s %d",
		t.Estado.Etapa, t.Mano[0], t.Mano[1], eq, rivales, sims, t.PorIgualar(), t.Estado.Pozo, a.Tipo, a.Monto)
}

// --- Preflop ----------------------------------------------------------------

func (b *botReadingSteiner) decidirPreflop(t botsdk.Turno, rivales int) botsdk.Accion {
	chen := puntuacionChen(t.Mano)
	pos := factorPosicion(t)
	costo := t.PorIgualar()
	bb := max(1, t.Estado.CiegaGrande)
	stack := t.Yo.Saldo
	costoRelStack := float64(costo) / float64(max(1, stack))
	subidas := subidasEnLaRonda(t)

	// Ajuste por posición y número de rivales.
	ajuste := pos
	if rivales >= 4 {
		ajuste -= 0.8
	} else if rivales >= 2 {
		ajuste -= 0.3
	}
	// Penalizar pagar subidas grandes con manos marginales.
	if subidas >= 2 {
		ajuste -= 1.2
	} else if subidas == 1 {
		ajuste -= 0.5
	}

	fuerza := chen + ajuste

	// Manos premium: subir/re-subir por valor.
	if chen >= 10.5 {
		return subirPreflop(t, bb, subidas, true)
	}

	// Manos fuertes.
	if fuerza >= 8.5 {
		if costoRelStack > 0.35 && chen < 11 {
			// No comprometer demasiado stack preflop sin premium.
			if t.Puede(botsdk.Check) {
				return t.Check()
			}
			if t.Puede(botsdk.Call) && costo <= bb*4 {
				return t.Pagar()
			}
			return t.Fold()
		}
		if puedeSubir(t) && (subidas == 0 || chen >= 9.5) {
			return subirPreflop(t, bb, subidas, false)
		}
		return pagarOCheck(t)
	}

	// Manos jugables: pagar barato, preferir posición.
	if fuerza >= 6.0 {
		if t.Puede(botsdk.Check) {
			// En posición tardía y sin subida, a veces abrir pequeño.
			if puedeSubir(t) && subidas == 0 && pos >= 0.5 && chen >= 6.5 && b.azar.Float64() < 0.35 {
				return t.Apostar(bb * 3)
			}
			return t.Check()
		}
		limite := bb * 3
		if pos >= 0.5 {
			limite = bb * 4
		}
		if t.Puede(botsdk.Call) && costo <= limite && costoRelStack <= 0.08 {
			return t.Pagar()
		}
		return t.Fold()
	}

	// Manos especulativas en posición tardía/barata.
	if fuerza >= 4.8 && pos >= 0.8 && subidas == 0 {
		if t.Puede(botsdk.Check) {
			if puedeSubir(t) && b.azar.Float64() < 0.15 {
				return t.Apostar(bb * 3)
			}
			return t.Check()
		}
		if t.Puede(botsdk.Call) && costo <= bb*2 && costoRelStack <= 0.04 {
			return t.Pagar()
		}
	}

	if t.Puede(botsdk.Check) {
		return t.Check()
	}
	return t.Fold()
}

func subirPreflop(t botsdk.Turno, bb int64, subidas int, premium bool) botsdk.Accion {
	if !puedeSubir(t) {
		return pagarOCheck(t)
	}
	// Tamaño base: 3BB + 1BB por cada call/subida ya vista.
	objetivo := t.Estado.ApuestaActual + bb*(3+int64(min(subidas, 3)))
	if premium {
		objetivo = t.Estado.ApuestaActual + bb*(4+int64(min(subidas, 3)))
	}
	// Asegurar mínimo de subida.
	if objetivo < t.SubidaMinima() {
		objetivo = t.SubidaMinima()
	}
	// Si el objetivo consume demasiado stack sin ser nuts absolutos, capear.
	if !premium {
		techo := t.Techo()
		maxCommit := t.Yo.ApuestaRonda + t.Yo.Saldo*2/5 // 40% stack
		if maxCommit < objetivo && maxCommit >= t.SubidaMinima() {
			objetivo = maxCommit
		}
		if objetivo > techo {
			objetivo = techo
		}
	}
	a := t.Apostar(objetivo)
	// Si Apostar devolvió all-in por accidente en spot no premium, preferir call/fold si caro.
	if a.Tipo == botsdk.AllIn && !premium && t.Yo.Saldo > bb*20 {
		if t.Puede(botsdk.Call) && t.PorIgualar() <= bb*6 {
			return t.Pagar()
		}
		if t.Puede(botsdk.Check) {
			return t.Check()
		}
	}
	return a
}

func factorPosicion(t botsdk.Turno) float64 {
	// Aproxima posición relativa al botón. 1 = tarde, 0 = temprano, 0.5 = media.
	n := len(t.Estado.Jugadores)
	if n <= 2 {
		return 1
	}
	// Encontrar índice del botón y del bot en la lista por silla.
	boton := t.Estado.PosicionBoton
	silla := t.Yo.PosicionSilla
	if silla < 0 {
		silla = t.Silla
	}
	if silla < 0 {
		return 0
	}
	// Distancia circular desde botón: 1 = SB, 2 = BB, ..., n = botón.
	dist := (silla - boton + n) % n
	// Normalizar: botón (dist 0) es mejor, SB (1) peor.
	if dist == 0 {
		return 1.0 // botón
	}
	// Cuanto más cerca del botón en orden de acción postflop, mejor.
	// En preflop SB/BB actúan últimos preflop pero son OOP postflop; penalizar.
	if dist == 1 { // SB
		return -0.3
	}
	if dist == 2 { // BB
		return -0.1
	}
	// Resto: mapear a [0.0, 0.8]
	rel := float64(dist-2) / float64(max(1, n-2))
	return rel * 0.8
}

func puntuacionChen(mano botsdk.Mano) float64 {
	alta, baja := mano[0].Rango, mano[1].Rango
	if baja > alta {
		alta, baja = baja, alta
	}
	puntaje := valorChen(alta)
	if alta == baja {
		puntaje *= 2
		if puntaje < 5 {
			puntaje = 5
		}
	}
	if mano[0].Palo == mano[1].Palo {
		puntaje += 2
	}
	hueco := int(alta) - int(baja) - 1
	switch {
	case hueco <= 0:
	case hueco == 1:
		puntaje -= 1
	case hueco == 2:
		puntaje -= 2
	case hueco == 3:
		puntaje -= 4
	default:
		puntaje -= 5
	}
	if hueco <= 1 && alta < botsdk.Reina && alta != baja {
		puntaje += 1
	}
	return puntaje
}

func valorChen(r botsdk.Rango) float64 {
	switch r {
	case botsdk.As:
		return 10
	case botsdk.Rey:
		return 8
	case botsdk.Reina:
		return 7
	case botsdk.Jota:
		return 6
	default:
		return float64(r) / 2
	}
}
// --- Postflop ---------------------------------------------------------------

func (b *botReadingSteiner) decidirPostflop(t botsdk.Turno, eq float64, rivales int) botsdk.Accion {
	costo := t.PorIgualar()
	pozo := t.Estado.Pozo
	bb := max(1, t.Estado.CiegaGrande)
	stack := max(1, t.Yo.Saldo)
	potOdds := 0.0
	if costo > 0 {
		potOdds = float64(min(costo, t.Yo.Saldo)) / float64(max(1, pozo+min(costo, t.Yo.Saldo)))
	}

	// Ajustes por etapa: ser más conservador early streets sin nuts.
	etapa := t.Estado.Etapa
	ajusteEtapa := 0.0
	switch etapa {
	case botsdk.Flop:
		ajusteEtapa = 0.03
	case botsdk.Turn:
		ajusteEtapa = 0.01
	case botsdk.River:
		ajusteEtapa = 0.0
	}

	// Amenaza del board: si hay cartas altas/posibles draws, exigir más margen.
	amenaza := amenazaBoard(t.Estado.Comunitarias)
	jugada := poker.Mejor(t.Mano, t.Estado.Comunitarias)

	// Penalizar equity efectiva en boards peligrosos si no tenemos la nuts.
	eqAdj := eq
	maxPalo := 0
	palos := map[botsdk.Palo]int{}
	for _, c := range t.Estado.Comunitarias { palos[c.Palo]++ }
	for _, n := range palos { if n > maxPalo { maxPalo = n } }
	tenemosPalo := 0
	for _, c := range t.Mano { if palos[c.Palo] > 0 { tenemosPalo = max(tenemosPalo, palos[c.Palo]) } }
	if maxPalo >= 3 && jugada.Categoria < poker.Color {
		// Board monotone: si no tenemos flush, ser mucho más cauteloso.
		if tenemosPalo+maxPalo < 5 { // ni siquiera draw decente
			eqAdj -= 0.08
		} else {
			eqAdj -= 0.04 // draw posible pero vulnerable
		}
	}
	// Board pareado: full house posible
	rangosB := map[botsdk.Rango]int{}
	for _, c := range t.Estado.Comunitarias { rangosB[c.Rango]++ }
	pareado := false
	for _, n := range rangosB { if n >= 2 { pareado = true } }
	if pareado && jugada.Categoria < poker.FullHouse && jugada.Categoria >= poker.Trio {
		eqAdj -= 0.03
	}

	margen := 0.10 + ajusteEtapa + amenaza
	if rivales >= 3 {
		margen += 0.05
	}
	if rivales == 1 {
		margen -= 0.01
	}

	valor := eqAdj - potOdds

	// Manos monstruo: apostar/subir por valor.
	if eqAdj >= 0.80 {
		return jugarValorFuerte(t, eq, rivales)
	}

	// Valor fuerte
	if eqAdj >= 0.64+ajusteEtapa {
		if puedeSubir(t) {
			fr := 0.60
			if eqAdj >= 0.72 {
				fr = 0.85
			}
			if eqAdj >= 0.78 {
				fr = 1.0
			}
			return apostarConControl(t, fr, bb, false)
		}
		return pagarOCheck(t)
	}

	// Valor medio / bluff-catch
	if valor >= margen {
		// Ante apuesta grande o subida, exigir más equity.
		if costo > 0 {
			presion := float64(costo) / float64(max(1, pozo))
			subidas := subidasEnLaRonda(t)
			if subidas >= 1 && eqAdj < 0.58 {
				if eqAdj >= potOdds+0.14 {
					return pagarOCheck(t)
				}
				return foldOCheck(t)
			}
			if presion >= 0.75 && eqAdj < 0.64 {
				if eqAdj >= potOdds+0.16 {
					return pagarOCheck(t)
				}
				return foldOCheck(t)
			}
			if presion >= 0.40 && eqAdj < 0.56 {
				if eqAdj >= potOdds+0.12 {
					return pagarOCheck(t)
				}
				return foldOCheck(t)
			}
		}
		if puedeSubir(t) && eqAdj >= 0.58 && costo == 0 && b.azar.Float64() < 0.45 {
			return apostarConControl(t, 0.50, bb, false)
		}
		return pagarOCheck(t)
	}

	// Draws/marginal con buenas odds implícitas: pagar barato.
	if costo > 0 && eqAdj >= potOdds+0.05 && eqAdj >= 0.32 {
		limite := int64(float64(pozo) * 0.40)
		if etapa == botsdk.River {
			limite = int64(float64(pozo) * 0.25)
		}
		if costo <= limite && float64(costo) <= float64(stack)*0.15 {
			return t.Pagar()
		}
	}

	// Mano hecha decente ante apuesta pequeña: no foldear demasiado.
	if costo > 0 && costo <= int64(float64(pozo)*0.25) {
		jugada := poker.Mejor(t.Mano, t.Estado.Comunitarias)
		if jugada.Categoria >= poker.Par {
			// En river, pagar apuestas pequeñas solo con par decente o mejor.
			if etapa == botsdk.River && jugada.Categoria == poker.Par && eqAdj < 0.45 {
				return foldOCheck(t)
			}
			return t.Pagar()
		}
	}

	// Sin costo: check o bluff pequeño ocasional.
	if costo == 0 {
		if puedeSubir(t) && rivales <= 2 && eq >= 0.20 && amenaza < 0.03 && b.azar.Float64() < 0.05 {
			return apostarConControl(t, 0.33, bb, true)
		}
		if t.Puede(botsdk.Check) {
			return t.Check()
		}
	}

	// Fold ante apuesta si no hay equity suficiente.
	if t.Puede(botsdk.Check) {
		return t.Check()
	}
	// Última defensa: si pot odds son absurdamente buenas, pagar.
	if costo > 0 && potOdds < 0.08 && eqAdj >= 0.18 {
		return t.Pagar()
	}
	return t.Fold()
}

func jugarValorFuerte(t botsdk.Turno, eq float64, rivales int) botsdk.Accion {
	bb := max(1, t.Estado.CiegaGrande)
	if puedeSubir(t) {
		fr := 0.75
		if eq >= 0.92 {
			fr = 1.2
		} else if eq >= 0.87 {
			fr = 1.0
		}
		return apostarConControl(t, fr, bb, false)
	}
	if t.Puede(botsdk.Call) {
		return t.Pagar()
	}
	if t.Puede(botsdk.Check) {
		return t.Check()
	}
	if t.Puede(botsdk.AllIn) && eq >= 0.90 {
		return t.AllIn()
	}
	return accionSegura(t)
}

func apostarConControl(t botsdk.Turno, fraccionPozo float64, bb int64, esBluff bool) botsdk.Accion {
	if !puedeSubir(t) {
		return pagarOCheck(t)
	}
	pozoBase := t.PozoSiPagas()
	if pozoBase <= 0 {
		pozoBase = t.Estado.Pozo
	}
	objetivo := t.Estado.ApuestaActual + int64(float64(max(1, pozoBase))*fraccionPozo)
	if objetivo < t.SubidaMinima() {
		objetivo = t.SubidaMinima()
	}

	// Evitar overcommit con bluff.
	if esBluff {
		maxBluff := t.Yo.ApuestaRonda + t.Yo.Saldo/4
		if objetivo > maxBluff {
			objetivo = maxBluff
		}
		if objetivo < t.SubidaMinima() {
			if t.Puede(botsdk.Check) {
				return t.Check()
			}
			return t.Fold()
		}
	}

	a := t.Apostar(objetivo)

	// Evitar all-in accidental con stack grande en bluffs/marginal.
	if a.Tipo == botsdk.AllIn && esBluff && t.Yo.Saldo > bb*15 {
		if t.Puede(botsdk.Check) {
			return t.Check()
		}
		return t.Fold()
	}
	return a
}

func amenazaBoard(comunitarias []botsdk.Carta) float64 {
	if len(comunitarias) < 3 {
		return 0
	}
	// Flush draw posible
	palos := map[botsdk.Palo]int{}
	for _, c := range comunitarias {
		palos[c.Palo]++
	}
	maxPalo := 0
	for _, n := range palos {
		if n > maxPalo {
			maxPalo = n
		}
	}
	amenaza := 0.0
	if maxPalo >= 4 {
		amenaza += 0.08
	} else if maxPalo >= 3 {
		amenaza += 0.03
	}

	// Cartas altas en board
	altas := 0
	for _, c := range comunitarias {
		if c.Rango >= botsdk.Jota {
			altas++
		}
	}
	if altas >= 3 {
		amenaza += 0.03
	} else if altas >= 2 {
		amenaza += 0.015
	}

	// Paired board
	rangos := map[botsdk.Rango]int{}
	for _, c := range comunitarias {
		rangos[c.Rango]++
	}
	for _, n := range rangos {
		if n >= 2 {
			amenaza += 0.02
			break
		}
	}
	return min(0.12, amenaza)
}

// --- Helpers de acción ------------------------------------------------------

func puedeSubir(t botsdk.Turno) bool {
	return t.Puede(botsdk.Bet) || t.Puede(botsdk.Raise) || t.Puede(botsdk.AllIn)
}

func pagarOCheck(t botsdk.Turno) botsdk.Accion {
	if t.Puede(botsdk.Check) {
		return t.Check()
	}
	if t.Puede(botsdk.Call) {
		return t.Pagar()
	}
	if t.Puede(botsdk.AllIn) && t.PorIgualar() >= t.Yo.Saldo {
		// Si solo queda all-in para igualar, ser cauteloso: el llamador debe decidir.
		// Aquí devolvemos all-in solo como fallback de "pagar".
		return t.AllIn()
	}
	return t.Fold()
}

func foldOCheck(t botsdk.Turno) botsdk.Accion {
	if t.Puede(botsdk.Check) {
		return t.Check()
	}
	return t.Fold()
}

func accionSegura(t botsdk.Turno) botsdk.Accion {
	if t.Puede(botsdk.Check) {
		return t.Check()
	}
	return t.Fold()
}

func contarRivales(t botsdk.Turno) int {
	rivales := 0
	for _, j := range t.Estado.Jugadores {
		if j.ID != t.IDJugador && j.Activo && j.EnMano {
			rivales++
		}
	}
	return rivales
}

func subidasEnLaRonda(t botsdk.Turno) int {
	n := 0
	for _, a := range t.Estado.HistorialAcciones {
		if a.Etapa != t.Estado.Etapa {
			continue
		}
		switch a.Accion.Tipo {
		case botsdk.Bet, botsdk.Raise, botsdk.AllIn:
			n++
		}
	}
	return n
}

func manoValida(mano botsdk.Mano) bool {
	return mano[0].Rango >= botsdk.Dos && mano[1].Rango >= botsdk.Dos && mano[0] != mano[1]
}

func presupuesto(plazo time.Duration) time.Duration {
	if plazo <= 0 {
		return presupuestoSinPlazo
	}
	p := plazo / fraccionDelPlazo
	if p > presupuestoMaximo {
		p = presupuestoMaximo
	}
	if p < 50*time.Millisecond {
		p = 50 * time.Millisecond
	}
	return p
}

// --- Monte Carlo ------------------------------------------------------------

type simulacion struct {
	mano            botsdk.Mano
	comunitarias    []botsdk.Carta
	rivales         int
	maxSimulaciones int
	presupuesto     time.Duration
}

type equidad struct {
	Valor        float64
	Simulaciones int
}

func estimarEquidad(ctx context.Context, azar *rand.Rand, sim simulacion) equidad {
	if sim.rivales <= 0 {
		return equidad{Valor: 1}
	}

	restantes := cartasRestantes(sim.mano, sim.comunitarias)
	faltanEnMesa := 5 - len(sim.comunitarias)
	if faltanEnMesa < 0 {
		faltanEnMesa = 0
	}
	if maximo := (len(restantes) - faltanEnMesa) / 2; sim.rivales > maximo {
		sim.rivales = maximo
	}
	if sim.rivales <= 0 {
		return equidad{Valor: 1}
	}
	necesarias := faltanEnMesa + 2*sim.rivales
	if necesarias > len(restantes) {
		return equidad{Valor: 0.5}
	}

	var tablero [5]botsdk.Carta
	copy(tablero[:], sim.comunitarias)

	limite := time.Now().Add(sim.presupuesto)
	var acumulado float64
	jugadas := 0
	for jugadas < sim.maxSimulaciones {
		if jugadas > 0 && jugadas%cadaCuantasReloj == 0 {
			if ctx.Err() != nil || time.Now().After(limite) {
				break
			}
		}
		for i := 0; i < necesarias; i++ {
			j := i + azar.IntN(len(restantes)-i)
			restantes[i], restantes[j] = restantes[j], restantes[i]
		}
		copy(tablero[len(sim.comunitarias):], restantes[:faltanEnMesa])

		acumulado += parteGanada(sim.mano, tablero[:], restantes[faltanEnMesa:necesarias])
		jugadas++
	}

	if jugadas == 0 {
		// Al menos una simulación si es posible.
		for i := 0; i < necesarias; i++ {
			j := i + azar.IntN(len(restantes)-i)
			restantes[i], restantes[j] = restantes[j], restantes[i]
		}
		copy(tablero[len(sim.comunitarias):], restantes[:faltanEnMesa])
		acumulado = parteGanada(sim.mano, tablero[:], restantes[faltanEnMesa:necesarias])
		jugadas = 1
	}

	return equidad{Valor: acumulado / float64(jugadas), Simulaciones: jugadas}
}

func parteGanada(mia botsdk.Mano, tablero []botsdk.Carta, manosRivales []botsdk.Carta) float64 {
	propio := poker.Mejor(mia, tablero).Puntaje
	empatados := 0
	for i := 0; i+1 < len(manosRivales); i += 2 {
		rival := poker.Mejor(botsdk.Mano{manosRivales[i], manosRivales[i+1]}, tablero).Puntaje
		if rival > propio {
			return 0
		}
		if rival == propio {
			empatados++
		}
	}
	return 1 / float64(empatados+1)
}

func cartasRestantes(mano botsdk.Mano, comunitarias []botsdk.Carta) []botsdk.Carta {
	var vistas [15][4]bool
	for _, c := range mano {
		if c.Rango >= botsdk.Dos && c.Rango <= botsdk.As && c.Palo >= botsdk.Treboles && c.Palo <= botsdk.Picas {
			vistas[c.Rango][c.Palo] = true
		}
	}
	for _, c := range comunitarias {
		if c.Rango >= botsdk.Dos && c.Rango <= botsdk.As && c.Palo >= botsdk.Treboles && c.Palo <= botsdk.Picas {
			vistas[c.Rango][c.Palo] = true
		}
	}

	restantes := make([]botsdk.Carta, 0, 52)
	for rango := botsdk.Dos; rango <= botsdk.As; rango++ {
		for palo := botsdk.Treboles; palo <= botsdk.Picas; palo++ {
			if !vistas[rango][palo] {
				restantes = append(restantes, botsdk.Carta{Rango: rango, Palo: palo})
			}
		}
	}
	return restantes
}

func main() { botsdk.CorrerDesdeFlags(&botReadingSteiner{}) }
