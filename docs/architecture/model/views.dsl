// Each view answers one question. Keep container views under ~15 elements;
// split a view before trying to style it into readability.

systemContext flodni "SystemContext" "What does Flodni depend on, and who depends on it?" {
    include *
    include viewer
    autoLayout lr
}

container flodni "Containers" "What runs, and which stores does it own?" {
    include *
    autoLayout lr
}

component flodni.daemon "Daemon" "What are the parts of the single process, and how does a read reach the swarm?" {
    include *
    exclude flodni.daemon.authGate
    exclude flodni.tokenFile
    exclude idProxy
    autoLayout tb 300 150
}

component flodni.daemon "Authentication" "Who may call the control API and the engine, and where is that checked?" {
    include flodni.daemon.control
    include flodni.daemon.engine
    include flodni.daemon.authGate
    include flodni.tokenFile
    include operator
    include automation
    include idProxy
    autoLayout tb 300 150
}
